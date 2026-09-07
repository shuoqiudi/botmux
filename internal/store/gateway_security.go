package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/skrashevich/botmux/internal/models"
)

var (
	ErrExpectedRevisionRequired = errors.New("expected_revision is required")
	ErrRevisionConflict         = errors.New("configuration revision conflict")
)

func (s *Store) migrateGatewaySecurity() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS gateway_audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			route_id INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 0,
			actor_kind TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			action TEXT NOT NULL,
			safe_diff TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_gateway_audit_route ON gateway_audit_events(route_id,id DESC);
		CREATE TRIGGER IF NOT EXISTS gateway_audit_append_only_update
		BEFORE UPDATE ON gateway_audit_events BEGIN SELECT RAISE(ABORT,'Gateway audit is append-only'); END;
		CREATE TRIGGER IF NOT EXISTS gateway_audit_append_only_delete
		BEFORE DELETE ON gateway_audit_events BEGIN SELECT RAISE(ABORT,'Gateway audit is append-only'); END;
	`)
	if err != nil {
		return err
	}
	if err := addColumnIfMissing(s.db, "gateway_workloads", "revision", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(s.db, "gateway_workload_credentials", "rotated_at", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func safeJSON(diff map[string]any) string {
	if diff == nil {
		return "{}"
	}
	raw, _ := json.Marshal(diff)
	return string(raw)
}

func appendGatewayAudit(exec sqlExecer, routeID, revision int64, actorKind, actorID, action string, diff map[string]any) error {
	if actorKind == "" {
		actorKind = "system"
	}
	if actorID == "" {
		actorID = "system"
	}
	_, err := exec.Exec(`INSERT INTO gateway_audit_events(route_id,revision,actor_kind,actor_id,action,safe_diff,created_at) VALUES(?,?,?,?,?,?,?)`,
		routeID, revision, actorKind, actorID, action, safeJSON(diff), nowRFC3339())
	return err
}

func (s *Store) RecordGatewayAudit(routeID, revision int64, actorID, action string, diff map[string]any) error {
	return appendGatewayAudit(s.db, routeID, revision, "admin", actorID, action, diff)
}

func (s *Store) GetGatewayAudit(routeKey string, limit int) ([]models.GatewayAuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT a.id,a.route_id,COALESCE(r.route_key,''),a.revision,a.actor_kind,a.actor_id,a.action,a.safe_diff,a.created_at
		FROM gateway_audit_events a LEFT JOIN gateway_business_routes r ON r.id=a.route_id`
	args := []any{}
	if routeKey != "" {
		query += ` WHERE r.route_key=?`
		args = append(args, routeKey)
	}
	query += ` ORDER BY a.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.GatewayAuditEvent{}
	for rows.Next() {
		var event models.GatewayAuditEvent
		var raw string
		if err := rows.Scan(&event.ID, &event.RouteID, &event.RouteKey, &event.Revision, &event.ActorKind, &event.ActorID, &event.Action, &raw, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &event.SafeDiff); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) UpdateBusinessRouteExpected(routeKey string, r models.BusinessRoute, expected int64, actorID string) error {
	if expected <= 0 {
		return ErrExpectedRevisionRequired
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, current int64
	var oldInbound, oldOutbound, oldEnabled bool
	var oldName string
	if err := tx.QueryRow(`SELECT id,revision,display_name,inbound_enabled,outbound_enabled,enabled FROM gateway_business_routes WHERE route_key=?`, routeKey).
		Scan(&id, &current, &oldName, &oldInbound, &oldOutbound, &oldEnabled); err != nil {
		return err
	}
	if current != expected {
		return ErrRevisionConflict
	}
	if r.RouteKey != "" && r.RouteKey != routeKey {
		return ErrRouteKeyImmutable
	}
	backendCiphertext, err := s.sealSecret(r.InboundBackendToken)
	if err != nil {
		return err
	}
	now, callers, next := nowRFC3339(), encodeCallers(r.AllowedCallers), current+1
	res, err := tx.Exec(`UPDATE gateway_business_routes SET display_name=?,bot_account_id=?,destination_id=?,inbound_enabled=?,inbound_backend_url=?,inbound_backend_token='',inbound_backend_token_ciphertext=?,outbound_enabled=?,allowed_callers=?,enabled=?,revision=?,status=?,last_validated_at=?,updated_at=? WHERE id=? AND revision=?`,
		r.DisplayName, r.BotAccountID, r.DestinationID, r.InboundEnabled, r.InboundBackendURL, backendCiphertext, r.OutboundEnabled, callers, r.Enabled, next, r.Status, r.LastValidatedAt, now, id, current)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRevisionConflict
	}
	if _, err := tx.Exec(`INSERT INTO gateway_backend_health_config(route_id,health_url) VALUES(?,?) ON CONFLICT(route_id) DO UPDATE SET health_url=excluded.health_url`, id, r.InboundBackendHealthURL); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO gateway_route_revisions(route_id,revision,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,validated_at,created_at) VALUES(?,?,?,?,?,?,?, '',?,?,?,?,?,?,?)`,
		id, next, r.DisplayName, r.BotAccountID, r.DestinationID, r.InboundEnabled, r.InboundBackendURL, backendCiphertext, r.OutboundEnabled, callers, r.Enabled, r.Status, r.LastValidatedAt, now); err != nil {
		return err
	}
	action := "route.update"
	if oldInbound != r.InboundEnabled || oldOutbound != r.OutboundEnabled || oldEnabled != r.Enabled {
		action = "route.disable"
	}
	diff := map[string]any{"display_name_changed": oldName != r.DisplayName, "inbound_enabled": []bool{oldInbound, r.InboundEnabled}, "outbound_enabled": []bool{oldOutbound, r.OutboundEnabled}, "enabled": []bool{oldEnabled, r.Enabled}, "backend_credential_changed": r.InboundBackendToken != ""}
	if err := appendGatewayAudit(tx, id, next, "admin", actorID, action, diff); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateBotAccount atomically installs a pre-validated candidate token and
// advances every affected Route revision. Validation must happen before this call.
func (s *Store) RotateBotAccount(id, expected int64, name, username, token, actorID string) error {
	if expected <= 0 {
		return ErrExpectedRevisionRequired
	}
	sealed, err := s.sealSecret(token)
	if err != nil {
		return err
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	var oldFingerprint string
	if err := tx.QueryRow(`SELECT revision,token_fingerprint FROM gateway_bot_accounts WHERE id=?`, id).Scan(&current, &oldFingerprint); err != nil {
		return err
	}
	if current != expected {
		return ErrRevisionConflict
	}
	if token == "" {
		_, err = tx.Exec(`UPDATE gateway_bot_accounts SET name=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, name, nowRFC3339(), id, current)
	} else {
		_, err = tx.Exec(`UPDATE gateway_bot_accounts SET name=?,username=?,token='',token_ciphertext=?,token_fingerprint=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, name, username, sealed, s.secretFingerprint(token), nowRFC3339(), id, current)
		if err == nil {
			_, err = tx.Exec(`UPDATE bots SET token='',token_ciphertext=?,token_fingerprint=?,bot_username=? WHERE token_fingerprint=?`, sealed, s.secretFingerprint(token), username, oldFingerprint)
		}
	}
	if err != nil {
		return err
	}
	if err := appendGatewayAudit(tx, 0, current+1, "admin", actorID, "bot_token.rotate", map[string]any{"bot_account_id": id, "token_changed": token != ""}); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id,revision FROM gateway_business_routes WHERE bot_account_id=?`, id)
	if err != nil {
		return err
	}
	type affected struct{ id, revision int64 }
	var routes []affected
	for rows.Next() {
		var a affected
		if err := rows.Scan(&a.id, &a.revision); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, a)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, route := range routes {
		next := route.revision + 1
		if _, err := tx.Exec(`UPDATE gateway_business_routes SET revision=?,updated_at=? WHERE id=? AND revision=?`, next, nowRFC3339(), route.id, route.revision); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO gateway_route_revisions(route_id,revision,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,validated_at,created_at)
			SELECT id,?,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,'',inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,last_validated_at,? FROM gateway_business_routes WHERE id=?`, next, nowRFC3339(), route.id); err != nil {
			return err
		}
		if err := appendGatewayAudit(tx, route.id, next, "admin", actorID, "bot_token.rotate", map[string]any{"token_changed": token != "", "bot_account_revision": current + 1}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) NativeBotIDsForGatewayAccount(id int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT b.id FROM bots b JOIN gateway_bot_accounts a ON a.token_fingerprint=b.token_fingerprint WHERE a.id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		ids = append(ids, value)
	}
	return ids, rows.Err()
}

// MigrateTelegramDestination atomically changes a pre-validated chat and pins
// the change in every affected Route revision without auditing the physical ID.
func (s *Store) MigrateTelegramDestination(d models.TelegramDestination, expected int64, actorID string) error {
	if expected <= 0 {
		return ErrExpectedRevisionRequired
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`SELECT revision FROM gateway_telegram_destinations WHERE id=?`, d.ID).Scan(&current); err != nil {
		return err
	}
	if current != expected {
		return ErrRevisionConflict
	}
	res, err := tx.Exec(`UPDATE gateway_telegram_destinations SET name=?,bot_account_id=?,chat_id=?,chat_title=?,status=?,validated_at=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, d.Name, d.BotAccountID, d.ChatID, d.ChatTitle, d.Status, d.ValidatedAt, nowRFC3339(), d.ID, current)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRevisionConflict
	}
	if err := appendGatewayAudit(tx, 0, current+1, "admin", actorID, "destination.migrate", map[string]any{"destination_id": d.ID, "chat_changed": true}); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id,revision FROM gateway_business_routes WHERE destination_id=?`, d.ID)
	if err != nil {
		return err
	}
	type affected struct{ id, revision int64 }
	var routes []affected
	for rows.Next() {
		var a affected
		if err := rows.Scan(&a.id, &a.revision); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, a)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, route := range routes {
		next := route.revision + 1
		// Keep every active Route reference internally consistent when a
		// Destination is rebound to another Bot Account. The old active revision
		// remains immutable and can still explain deliveries accepted before the
		// migration.
		result, err := tx.Exec(`UPDATE gateway_business_routes SET bot_account_id=?,revision=?,last_validated_at=?,updated_at=? WHERE id=? AND revision=?`, d.BotAccountID, next, d.ValidatedAt, nowRFC3339(), route.id, route.revision)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return ErrRevisionConflict
		}
		if _, err := tx.Exec(`INSERT INTO gateway_route_revisions(route_id,revision,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,validated_at,created_at)
			SELECT id,?,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,'',inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,?,? FROM gateway_business_routes WHERE id=?`, next, d.ValidatedAt, nowRFC3339(), route.id); err != nil {
			return err
		}
		if err := appendGatewayAudit(tx, route.id, next, "admin", actorID, "destination.migrate", map[string]any{"destination_revision": current + 1, "chat_changed": true}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CreateGatewayWorkloadAdmin(name, credentialName, credentialHash, actorID string) (*models.GatewayWorkloadAdmin, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	res, err := tx.Exec(`INSERT INTO gateway_workloads(name,status,revision,created_at,updated_at) VALUES(?,'active',1,?,?)`, name, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO gateway_workload_credentials(workload_id,name,credential_hash,created_at) VALUES(?,?,?,?)`, id, credentialName, credentialHash, now); err != nil {
		return nil, err
	}
	if err := appendGatewayAudit(tx, 0, 1, "admin", actorID, "workload.create", map[string]any{"workload_id": id, "credential_created": true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetGatewayWorkloadAdmin(id)
}

func (s *Store) GetGatewayWorkloadAdmin(id int64) (*models.GatewayWorkloadAdmin, error) {
	var w models.GatewayWorkloadAdmin
	if err := s.db.QueryRow(`SELECT id,name,status,revision,created_at,updated_at FROM gateway_workloads WHERE id=?`, id).Scan(&w.ID, &w.Name, &w.Status, &w.Revision, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,name,enabled,created_at,last_used_at,rotated_at FROM gateway_workload_credentials WHERE workload_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c models.GatewayWorkloadCredential
		if err := rows.Scan(&c.ID, &c.Name, &c.Enabled, &c.CreatedAt, &c.LastUsedAt, &c.RotatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		w.Credentials = append(w.Credentials, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.Query(`SELECT r.route_key,p.action FROM gateway_route_permissions p JOIN gateway_business_routes r ON r.id=p.route_id WHERE p.workload_id=? ORDER BY r.route_key,p.action`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p models.GatewayPermission
		if err := rows.Scan(&p.RouteKey, &p.Action); err != nil {
			rows.Close()
			return nil, err
		}
		w.Permissions = append(w.Permissions, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if w.Credentials == nil {
		w.Credentials = []models.GatewayWorkloadCredential{}
	}
	if w.Permissions == nil {
		w.Permissions = []models.GatewayPermission{}
	}
	return &w, nil
}

func (s *Store) GetGatewayWorkloadsAdmin() ([]models.GatewayWorkloadAdmin, error) {
	rows, err := s.db.Query(`SELECT id FROM gateway_workloads ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := []models.GatewayWorkloadAdmin{}
	for _, id := range ids {
		w, err := s.GetGatewayWorkloadAdmin(id)
		if err != nil {
			return nil, err
		}
		result = append(result, *w)
	}
	return result, nil
}

func (s *Store) RotateGatewayWorkloadCredential(id, expected int64, name, hash, actorID string) (*models.GatewayWorkloadAdmin, error) {
	if expected <= 0 {
		return nil, ErrExpectedRevisionRequired
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`SELECT revision FROM gateway_workloads WHERE id=?`, id).Scan(&current); err != nil {
		return nil, err
	}
	if current != expected {
		return nil, ErrRevisionConflict
	}
	now := nowRFC3339()
	if _, err := tx.Exec(`UPDATE gateway_workload_credentials SET enabled=0,rotated_at=? WHERE workload_id=? AND enabled=1`, now, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO gateway_workload_credentials(workload_id,name,credential_hash,created_at) VALUES(?,?,?,?)`, id, name, hash, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE gateway_workloads SET revision=revision+1,updated_at=? WHERE id=?`, now, id); err != nil {
		return nil, err
	}
	if err := appendGatewayAudit(tx, 0, current+1, "admin", actorID, "credential.rotate", map[string]any{"workload_id": id, "credential_changed": true}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetGatewayWorkloadAdmin(id)
}

func (s *Store) SetGatewayPermissions(id, expected int64, permissions []models.GatewayPermission, actorID string) (*models.GatewayWorkloadAdmin, error) {
	if expected <= 0 {
		return nil, ErrExpectedRevisionRequired
	}
	normalized := append([]models.GatewayPermission(nil), permissions...)
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].RouteKey+string(normalized[i].Action) < normalized[j].RouteKey+string(normalized[j].Action)
	})
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`SELECT revision FROM gateway_workloads WHERE id=?`, id).Scan(&current); err != nil {
		return nil, err
	}
	if current != expected {
		return nil, ErrRevisionConflict
	}
	if _, err := tx.Exec(`DELETE FROM gateway_route_permissions WHERE workload_id=?`, id); err != nil {
		return nil, err
	}
	for _, p := range normalized {
		if err := validateGatewayAction(p.Action); err != nil {
			return nil, err
		}
		res, err := tx.Exec(`INSERT INTO gateway_route_permissions(workload_id,route_id,action,created_at) SELECT ?,id,?,? FROM gateway_business_routes WHERE route_key=?`, id, p.Action, nowRFC3339(), p.RouteKey)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, sql.ErrNoRows
		}
	}
	if _, err := tx.Exec(`UPDATE gateway_workloads SET revision=revision+1,updated_at=? WHERE id=?`, nowRFC3339(), id); err != nil {
		return nil, err
	}
	safe := make([]string, 0, len(normalized))
	for _, p := range normalized {
		safe = append(safe, p.RouteKey+":"+string(p.Action))
	}
	if err := appendGatewayAudit(tx, 0, current+1, "admin", actorID, "permissions.replace", map[string]any{"workload_id": id, "grants": safe}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetGatewayWorkloadAdmin(id)
}

func (s *Store) SetGatewayWorkloadStatus(id, expected int64, status, actorID string) (*models.GatewayWorkloadAdmin, error) {
	if expected <= 0 {
		return nil, ErrExpectedRevisionRequired
	}
	if status != "active" && status != "disabled" {
		return nil, errors.New("status must be active or disabled")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`SELECT revision FROM gateway_workloads WHERE id=?`, id).Scan(&current); err != nil {
		return nil, err
	}
	if current != expected {
		return nil, ErrRevisionConflict
	}
	if _, err := tx.Exec(`UPDATE gateway_workloads SET status=?,revision=revision+1,updated_at=? WHERE id=?`, status, nowRFC3339(), id); err != nil {
		return nil, err
	}
	if err := appendGatewayAudit(tx, 0, current+1, "admin", actorID, "workload.disable", map[string]any{"workload_id": id, "status": status}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetGatewayWorkloadAdmin(id)
}
