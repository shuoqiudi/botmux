package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

var ErrGatewayIdempotencyConflict = errors.New("idempotency key was already used with a different payload")

func (s *Store) migrateGatewayOutbound() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS gateway_workloads (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS gateway_workload_credentials (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workload_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			credential_hash TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			last_used_at TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(workload_id) REFERENCES gateway_workloads(id)
		);
		CREATE TABLE IF NOT EXISTS gateway_route_permissions (
			workload_id INTEGER NOT NULL,
			route_id INTEGER NOT NULL,
			action TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(workload_id, route_id, action),
			FOREIGN KEY(workload_id) REFERENCES gateway_workloads(id),
			FOREIGN KEY(route_id) REFERENCES gateway_business_routes(id)
		);
		CREATE TABLE IF NOT EXISTS gateway_deliveries (
			id TEXT PRIMARY KEY,
			route_id INTEGER NOT NULL,
			route_revision INTEGER NOT NULL,
			workload_id INTEGER NOT NULL,
			direction TEXT NOT NULL,
			action TEXT NOT NULL,
			status TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			safe_error_class TEXT NOT NULL DEFAULT '',
			next_attempt_at TEXT NOT NULL DEFAULT '',
			telegram_message_id INTEGER,
			created_at TEXT NOT NULL,
			accepted_at TEXT NOT NULL DEFAULT '',
			completed_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			UNIQUE(workload_id, route_id, idempotency_key),
			FOREIGN KEY(route_id) REFERENCES gateway_business_routes(id),
			FOREIGN KEY(workload_id) REFERENCES gateway_workloads(id)
		);
		CREATE INDEX IF NOT EXISTS idx_gateway_deliveries_route_created
			ON gateway_deliveries(route_id, created_at DESC);
		CREATE TABLE IF NOT EXISTS gateway_delivery_payloads (
			delivery_id TEXT PRIMARY KEY,
			payload_json BLOB NOT NULL,
			FOREIGN KEY(delivery_id) REFERENCES gateway_deliveries(id)
		);
		CREATE TABLE IF NOT EXISTS gateway_delivery_attempts (
			attempt_id TEXT PRIMARY KEY,
			delivery_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			worker_id TEXT NOT NULL,
			error_class TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			ended_at TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(delivery_id) REFERENCES gateway_deliveries(id),
			UNIQUE(delivery_id, sequence)
		);
		CREATE TABLE IF NOT EXISTS gateway_dlq (
			delivery_id TEXT PRIMARY KEY,
			route_id INTEGER NOT NULL,
			source_stream_id TEXT NOT NULL,
			error_class TEXT NOT NULL,
			attempt_count INTEGER NOT NULL,
			entered_at TEXT NOT NULL,
			FOREIGN KEY(delivery_id) REFERENCES gateway_deliveries(id),
			FOREIGN KEY(route_id) REFERENCES gateway_business_routes(id)
		);
		CREATE TABLE IF NOT EXISTS gateway_bot_rate_limits (
			bot_account_id INTEGER PRIMARY KEY,
			blocked_until TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(bot_account_id) REFERENCES gateway_bot_accounts(id)
		);
	`)
	if err != nil {
		return err
	}
	// Existing Gateway databases predate retry scheduling. SQLite has no
	// portable ADD COLUMN IF NOT EXISTS, so inspect before upgrading.
	rows, err := s.db.Query(`PRAGMA table_info(gateway_deliveries)`)
	if err != nil {
		return err
	}
	hasNextAttempt := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		hasNextAttempt = hasNextAttempt || name == "next_attempt_at"
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasNextAttempt {
		_, err = s.db.Exec(`ALTER TABLE gateway_deliveries ADD COLUMN next_attempt_at TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, ddl string }{
		{"state", "TEXT NOT NULL DEFAULT 'active'"},
		{"replay_count", "INTEGER NOT NULL DEFAULT 0"},
		{"acted_at", "TEXT NOT NULL DEFAULT ''"},
		{"acted_by", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(s.db, "gateway_dlq", column.name, column.ddl); err != nil {
			return err
		}
	}
	return nil
}

// CreateGatewayWorkload provisions a workload credential hash. The plaintext
// credential exists only in the caller that generated it.
func (s *Store) CreateGatewayWorkload(name, credentialName, credentialHash string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	res, err := tx.Exec(`INSERT INTO gateway_workloads(name,status,created_at,updated_at) VALUES(?,'active',?,?)`, name, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO gateway_workload_credentials(workload_id,name,credential_hash,created_at) VALUES(?,?,?,?)`, id, credentialName, credentialHash, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) GrantGatewayPermission(workloadID int64, routeKey string, action models.GatewayAction) error {
	if err := validateGatewayAction(action); err != nil {
		return err
	}
	result, err := s.db.Exec(`INSERT OR IGNORE INTO gateway_route_permissions(workload_id,route_id,action,created_at)
		SELECT ?,id,?,? FROM gateway_business_routes WHERE route_key=?`, workloadID, action, nowRFC3339(), routeKey)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM gateway_business_routes WHERE route_key=?`, routeKey).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return sql.ErrNoRows
		}
	}
	return nil
}

func (s *Store) AuthenticateGatewayWorkload(ctx context.Context, credentialHash string) (*models.GatewayWorkload, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	var workload models.GatewayWorkload
	var credentialID int64
	err := s.db.QueryRowContext(ctx, `SELECT w.id,w.name,w.status,c.id
		FROM gateway_workloads w JOIN gateway_workload_credentials c ON c.workload_id=w.id
		WHERE c.credential_hash=? AND c.enabled=1 AND w.status='active' AND w.id>0`, credentialHash).
		Scan(&workload.ID, &workload.Name, &workload.Status, &credentialID)
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE gateway_workload_credentials SET last_used_at=? WHERE id=?`, nowRFC3339(), credentialID)
	return &workload, nil
}

func (s *Store) GatewayRouteForAction(ctx context.Context, workloadID int64, routeKey string, action models.GatewayAction) (*models.BusinessRoute, bool, error) {
	route, err := s.GetBusinessRoute(routeKey)
	if err != nil {
		return nil, false, err
	}
	var allowed int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_route_permissions WHERE workload_id=? AND route_id=? AND action=?`, workloadID, route.ID, action).Scan(&allowed)
	if err != nil {
		return nil, false, err
	}
	return route, allowed == 1, nil
}

func scanGatewayDelivery(scanner interface{ Scan(...any) error }) (*models.GatewayDelivery, error) {
	var d models.GatewayDelivery
	err := scanner.Scan(&d.ID, &d.RouteID, &d.RouteKey, &d.RouteRevision, &d.WorkloadID, &d.Direction,
		&d.Action, &d.Status, &d.AttemptCount, &d.SafeErrorClass, &d.CreatedAt, &d.AcceptedAt,
		&d.NextAttemptAt, &d.CompletedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

const gatewayDeliverySelect = `SELECT d.id,d.route_id,r.route_key,d.route_revision,d.workload_id,d.direction,
	d.action,d.status,d.attempt_count,d.safe_error_class,d.created_at,d.accepted_at,d.next_attempt_at,d.completed_at,d.updated_at
	FROM gateway_deliveries d JOIN gateway_business_routes r ON r.id=d.route_id`

func (s *Store) CreateGatewayOutboundDelivery(ctx context.Context, in models.GatewayDeliveryCreate) (*models.GatewayDelivery, bool, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_deliveries
		(id,route_id,route_revision,workload_id,direction,action,status,payload_hash,idempotency_key,created_at,updated_at)
		VALUES(?,?,?,?,'outbound',?,'pending_enqueue',?,?,?,?)`, in.ID, in.RouteID, in.RouteRevision,
		in.WorkloadID, in.Action, in.PayloadHash, in.IdempotencyKey, now, now)
	if err != nil {
		var existingID, existingHash string
		lookupErr := tx.QueryRowContext(ctx, `SELECT id,payload_hash FROM gateway_deliveries WHERE workload_id=? AND route_id=? AND idempotency_key=?`,
			in.WorkloadID, in.RouteID, in.IdempotencyKey).Scan(&existingID, &existingHash)
		if lookupErr != nil {
			return nil, false, err
		}
		if existingHash != in.PayloadHash {
			return nil, false, ErrGatewayIdempotencyConflict
		}
		delivery, lookupErr := scanGatewayDelivery(tx.QueryRowContext(ctx, gatewayDeliverySelect+` WHERE d.id=?`, existingID))
		return delivery, false, lookupErr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_delivery_payloads(delivery_id,payload_json) VALUES(?,?)`, in.ID, in.PayloadJSON); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	delivery, err := scanGatewayDelivery(s.db.QueryRowContext(ctx, gatewayDeliverySelect+` WHERE d.route_id=? AND d.id=?`, in.RouteID, in.ID))
	return delivery, true, err
}

func (s *Store) MarkGatewayDeliveryAccepted(ctx context.Context, id string) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	now := nowRFC3339()
	_, err := s.db.ExecContext(ctx, `UPDATE gateway_deliveries SET status=CASE WHEN status='pending_enqueue' THEN 'accepted' ELSE status END,
		accepted_at=CASE WHEN accepted_at='' THEN ? ELSE accepted_at END,updated_at=? WHERE id=?`, now, now, id)
	return err
}

func (s *Store) GetGatewayDelivery(ctx context.Context, routeID int64, id string) (*models.GatewayDelivery, error) {
	return scanGatewayDelivery(s.db.QueryRowContext(ctx, gatewayDeliverySelect+` WHERE d.route_id=? AND d.id=?`, routeID, id))
}

func (s *Store) GetGatewayDeliveryForWorker(ctx context.Context, id string) (*models.GatewayDelivery, []byte, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	delivery, err := scanGatewayDelivery(s.db.QueryRowContext(ctx, gatewayDeliverySelect+` WHERE d.id=?`, id))
	if err != nil {
		return nil, nil, err
	}
	var payload []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload_json FROM gateway_delivery_payloads WHERE delivery_id=?`, id).Scan(&payload); err != nil {
		return nil, nil, err
	}
	return delivery, payload, nil
}

func (s *Store) ResolveGatewayOutboundTarget(ctx context.Context, routeID int64) (*models.GatewayOutboundTarget, error) {
	var target models.GatewayOutboundTarget
	var tokenCiphertext string
	err := s.db.QueryRowContext(ctx, `SELECT r.route_key,a.id,r.enabled,r.outbound_enabled,a.token_ciphertext,d.chat_id
		FROM gateway_business_routes r
		JOIN gateway_bot_accounts a ON a.id=r.bot_account_id
		JOIN gateway_telegram_destinations d ON d.id=r.destination_id
		WHERE r.id=?`, routeID).Scan(&target.RouteKey, &target.BotAccountID, &target.Enabled, &target.Outbound, &tokenCiphertext, &target.ChatID)
	if err != nil {
		return nil, err
	}
	target.Token, err = s.openSecret(tokenCiphertext)
	if err != nil {
		return nil, err
	}
	return &target, nil
}

func (s *Store) BeginGatewayDeliveryAttempt(ctx context.Context, id, attemptID, workerID string) (int, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	result, err := tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='processing',attempt_count=attempt_count+1,
		next_attempt_at='',updated_at=? WHERE id=? AND status IN ('accepted','retrying')`, now, id)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed != 1 {
		return 0, sql.ErrNoRows
	}
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT attempt_count FROM gateway_deliveries WHERE id=?`, id).Scan(&attempt); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_delivery_attempts(attempt_id,delivery_id,sequence,worker_id,started_at)
		VALUES(?,?,?,?,?)`, attemptID, id, attempt, workerID, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return attempt, nil
}

func (s *Store) MarkGatewayDeliverySucceeded(ctx context.Context, id string, telegramMessageID *int64) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	now := nowRFC3339()
	_, err := s.db.ExecContext(ctx, `UPDATE gateway_deliveries SET status='succeeded',safe_error_class='',next_attempt_at='',telegram_message_id=?,completed_at=?,updated_at=? WHERE id=?`, telegramMessageID, now, now, id)
	return err
}

func (s *Store) MarkGatewayDeliveryRetrying(ctx context.Context, id, attemptID, safeClass string, next time.Time) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	now := nowRFC3339()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='retrying',safe_error_class=?,next_attempt_at=?,updated_at=? WHERE id=?`, safeClass, next.UTC().Format(time.RFC3339Nano), now, id); err != nil {
		return err
	}
	if attemptID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE gateway_delivery_attempts SET error_class=?,ended_at=? WHERE attempt_id=?`, safeClass, now, attemptID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) MarkGatewayDeliveryReconciling(ctx context.Context, id, attemptID, safeClass string) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	now := nowRFC3339()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='reconciling',safe_error_class=?,next_attempt_at='',updated_at=? WHERE id=?`, safeClass, now, id); err != nil {
		return err
	}
	if attemptID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE gateway_delivery_attempts SET error_class=?,ended_at=? WHERE attempt_id=?`, safeClass, now, attemptID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) MarkGatewayDeliveryDeadLettered(ctx context.Context, id, attemptID, sourceID, safeClass string, attempts int) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_dlq(delivery_id,route_id,source_stream_id,error_class,attempt_count,entered_at,state,acted_at,acted_by)
		SELECT id,route_id,?,?,?,?,'active','','' FROM gateway_deliveries WHERE id=?
		ON CONFLICT(delivery_id) DO UPDATE SET source_stream_id=excluded.source_stream_id,error_class=excluded.error_class,
		attempt_count=excluded.attempt_count,entered_at=excluded.entered_at,state='active',acted_at='',acted_by=''`, sourceID, safeClass, attempts, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='dead-lettered',safe_error_class=?,next_attempt_at='',completed_at=?,updated_at=? WHERE id=?`, safeClass, now, now, id); err != nil {
		return err
	}
	if attemptID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE gateway_delivery_attempts SET error_class=?,ended_at=? WHERE attempt_id=?`, safeClass, now, attemptID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CompleteGatewayDeliveryAttempt(ctx context.Context, attemptID string) error {
	if attemptID == "" {
		return nil
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE gateway_delivery_attempts SET ended_at=? WHERE attempt_id=?`, nowRFC3339(), attemptID)
	return err
}

func (s *Store) GatewayBotRateLimit(ctx context.Context, botID int64) (time.Time, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT blocked_until FROM gateway_bot_rate_limits WHERE bot_account_id=?`, botID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid stored bot rate-limit deadline: %w", err)
	}
	return deadline, nil
}

func (s *Store) SetGatewayBotRateLimit(ctx context.Context, botID int64, deadline time.Time) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	until := deadline.UTC().Format(time.RFC3339Nano)
	var current string
	err := s.db.QueryRowContext(ctx, `SELECT blocked_until FROM gateway_bot_rate_limits WHERE bot_account_id=?`, botID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		stored, parseErr := time.Parse(time.RFC3339Nano, current)
		if parseErr != nil {
			return fmt.Errorf("invalid stored bot rate-limit deadline: %w", parseErr)
		}
		if stored.After(deadline) {
			return nil
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO gateway_bot_rate_limits(bot_account_id,blocked_until,updated_at)
		VALUES(?,?,?) ON CONFLICT(bot_account_id) DO UPDATE SET
		blocked_until=excluded.blocked_until,updated_at=excluded.updated_at`, botID, until, nowRFC3339())
	return err
}

func (s *Store) GatewayOutboundCounts(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_deliveries WHERE direction='outbound'`).Scan(&count)
	return count, err
}

func validateGatewayAction(action models.GatewayAction) error {
	switch action {
	case models.GatewayActionMessagesSend, models.GatewayActionCallbacksAnswer, models.GatewayActionDeliveriesRead:
		return nil
	default:
		return fmt.Errorf("unsupported gateway action")
	}
}
