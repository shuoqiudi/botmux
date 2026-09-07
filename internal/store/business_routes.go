package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

var ErrRouteKeyImmutable = errors.New("route_key is immutable")

func (s *Store) migrateBusinessRoutes() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS gateway_bot_accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			username TEXT NOT NULL,
			token TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS gateway_telegram_destinations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			bot_account_id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			chat_title TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			validated_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(bot_account_id, chat_id),
			FOREIGN KEY(bot_account_id) REFERENCES gateway_bot_accounts(id)
		);
		CREATE TABLE IF NOT EXISTS gateway_business_routes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			route_key TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			bot_account_id INTEGER NOT NULL,
			destination_id INTEGER NOT NULL,
			inbound_enabled INTEGER NOT NULL DEFAULT 0,
			inbound_backend_url TEXT NOT NULL DEFAULT '',
			inbound_backend_token TEXT NOT NULL DEFAULT '',
			outbound_enabled INTEGER NOT NULL DEFAULT 1,
			allowed_callers TEXT NOT NULL DEFAULT '[]',
			enabled INTEGER NOT NULL DEFAULT 1,
			revision INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'active',
			last_validated_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(bot_account_id) REFERENCES gateway_bot_accounts(id),
			FOREIGN KEY(destination_id) REFERENCES gateway_telegram_destinations(id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_business_routes_inbound_destination
			ON gateway_business_routes(bot_account_id, destination_id)
			WHERE inbound_enabled=1 AND enabled=1;
		CREATE TABLE IF NOT EXISTS gateway_route_revisions (
			route_id INTEGER NOT NULL,
			revision INTEGER NOT NULL,
			display_name TEXT NOT NULL,
			bot_account_id INTEGER NOT NULL,
			destination_id INTEGER NOT NULL,
			inbound_enabled INTEGER NOT NULL,
			inbound_backend_url TEXT NOT NULL,
			inbound_backend_token TEXT NOT NULL DEFAULT '',
			outbound_enabled INTEGER NOT NULL,
			allowed_callers TEXT NOT NULL,
			enabled INTEGER NOT NULL,
			status TEXT NOT NULL,
			validated_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(route_id, revision)
		);
		CREATE TRIGGER IF NOT EXISTS gateway_business_route_key_immutable
		BEFORE UPDATE OF route_key ON gateway_business_routes
		WHEN NEW.route_key <> OLD.route_key
		BEGIN
			SELECT RAISE(ABORT, 'route_key is immutable');
		END;
	`)
	if err != nil {
		return err
	}
	// Existing Gateway databases predate authenticated inbound delivery.
	for _, table := range []string{"gateway_business_routes", "gateway_route_revisions"} {
		var present int
		if err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name='inbound_backend_token'`, table)).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN inbound_backend_token TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	return s.migrateInboundDeliveries()
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) AddBotAccount(a models.BotAccount) (int64, error) {
	now := nowRFC3339()
	sealed, err := s.sealSecret(a.Token)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO gateway_bot_accounts(name,username,token,token_ciphertext,token_fingerprint,revision,created_at,updated_at) VALUES(?,?, '',?,?,1,?,?)`,
		a.Name, a.Username, sealed, s.secretFingerprint(a.Token), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateBotAccount(a models.BotAccount) error {
	if a.Token == "" {
		_, err := s.db.Exec(`UPDATE gateway_bot_accounts SET name=?, username=?, updated_at=? WHERE id=?`, a.Name, a.Username, nowRFC3339(), a.ID)
		return err
	}
	sealed, err := s.sealSecret(a.Token)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE gateway_bot_accounts SET name=?,username=?,token='',token_ciphertext=?,token_fingerprint=?,revision=revision+1,updated_at=? WHERE id=?`, a.Name, a.Username, sealed, s.secretFingerprint(a.Token), nowRFC3339(), a.ID)
	return err
}

func (s *Store) GetBotAccount(id int64) (*models.BotAccount, error) {
	var a models.BotAccount
	var ciphertext string
	err := s.db.QueryRow(`SELECT id,name,username,token_ciphertext,revision,created_at,updated_at FROM gateway_bot_accounts WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Username, &ciphertext, &a.Revision, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.Token, err = s.openSecret(ciphertext)
	if err != nil {
		return nil, err
	}
	a.TokenSet = ciphertext != ""
	a.TokenConfigured = a.TokenSet
	return &a, nil
}

func (s *Store) GetBotAccounts() ([]models.BotAccount, error) {
	rows, err := s.db.Query(`SELECT id,name,username,token_ciphertext,revision,created_at,updated_at FROM gateway_bot_accounts ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.BotAccount{}
	for rows.Next() {
		var a models.BotAccount
		var ciphertext string
		if err := rows.Scan(&a.ID, &a.Name, &a.Username, &ciphertext, &a.Revision, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Token, err = s.openSecret(ciphertext)
		if err != nil {
			return nil, err
		}
		a.TokenSet = ciphertext != ""
		a.TokenConfigured = a.TokenSet
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) AddTelegramDestination(d models.TelegramDestination) (int64, error) {
	now := nowRFC3339()
	res, err := s.db.Exec(`INSERT INTO gateway_telegram_destinations(name,bot_account_id,chat_id,chat_title,status,validated_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		d.Name, d.BotAccountID, d.ChatID, d.ChatTitle, d.Status, d.ValidatedAt, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateTelegramDestination(d models.TelegramDestination) error {
	_, err := s.db.Exec(`UPDATE gateway_telegram_destinations SET name=?,bot_account_id=?,chat_id=?,chat_title=?,status=?,validated_at=?,revision=revision+1,updated_at=? WHERE id=?`,
		d.Name, d.BotAccountID, d.ChatID, d.ChatTitle, d.Status, d.ValidatedAt, nowRFC3339(), d.ID)
	return err
}

func (s *Store) GetTelegramDestination(id int64) (*models.TelegramDestination, error) {
	var d models.TelegramDestination
	err := s.db.QueryRow(`SELECT id,name,bot_account_id,chat_id,chat_title,status,validated_at,revision,created_at,updated_at FROM gateway_telegram_destinations WHERE id=?`, id).
		Scan(&d.ID, &d.Name, &d.BotAccountID, &d.ChatID, &d.ChatTitle, &d.Status, &d.ValidatedAt, &d.Revision, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) GetTelegramDestinations() ([]models.TelegramDestination, error) {
	rows, err := s.db.Query(`SELECT id,name,bot_account_id,chat_id,chat_title,status,validated_at,revision,created_at,updated_at FROM gateway_telegram_destinations ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.TelegramDestination{}
	for rows.Next() {
		var d models.TelegramDestination
		if err := rows.Scan(&d.ID, &d.Name, &d.BotAccountID, &d.ChatID, &d.ChatTitle, &d.Status, &d.ValidatedAt, &d.Revision, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func encodeCallers(callers []string) string {
	if callers == nil {
		callers = []string{}
	}
	b, _ := json.Marshal(callers)
	return string(b)
}

func (s *Store) scanBusinessRoute(scanner interface{ Scan(...any) error }) (*models.BusinessRoute, error) {
	var r models.BusinessRoute
	var callers, backendCiphertext string
	err := scanner.Scan(&r.ID, &r.RouteKey, &r.DisplayName, &r.BotAccountID, &r.DestinationID,
		&r.InboundEnabled, &r.InboundBackendURL, &backendCiphertext, &r.OutboundEnabled, &callers, &r.Enabled,
		&r.Revision, &r.Status, &r.LastValidatedAt, &r.CreatedAt, &r.UpdatedAt,
		&r.BotAccountName, &r.BotUsername, &r.DestinationName, &r.DestinationChatID)
	if err != nil {
		return nil, err
	}
	r.InboundBackendToken, err = s.openSecret(backendCiphertext)
	if err != nil {
		return nil, err
	}
	r.BackendCredentialSet = backendCiphertext != ""
	r.CredentialConfigured = r.BackendCredentialSet
	if err := json.Unmarshal([]byte(callers), &r.AllowedCallers); err != nil {
		return nil, err
	}
	if r.AllowedCallers == nil {
		r.AllowedCallers = []string{}
	}
	r.Path = "/api/v1/routes/" + r.RouteKey + "/messages"
	return &r, nil
}

const businessRouteSelect = `
	SELECT r.id,r.route_key,r.display_name,r.bot_account_id,r.destination_id,
		r.inbound_enabled,r.inbound_backend_url,r.inbound_backend_token_ciphertext,r.outbound_enabled,r.allowed_callers,r.enabled,
		r.revision,r.status,r.last_validated_at,r.created_at,r.updated_at,
		a.name,a.username,d.name,d.chat_id
	FROM gateway_business_routes r
	JOIN gateway_bot_accounts a ON a.id=r.bot_account_id
	JOIN gateway_telegram_destinations d ON d.id=r.destination_id`

func (s *Store) AddBusinessRoute(r models.BusinessRoute) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := insertBusinessRoute(s, tx, r)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

type sqlExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func insertBusinessRoute(s *Store, exec sqlExecer, r models.BusinessRoute) (int64, error) {
	now := nowRFC3339()
	callers := encodeCallers(r.AllowedCallers)
	backendCiphertext, err := s.sealSecret(r.InboundBackendToken)
	if err != nil {
		return 0, err
	}
	res, err := exec.Exec(`INSERT INTO gateway_business_routes(route_key,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,revision,status,last_validated_at,created_at,updated_at) VALUES(?,?,?,?,?,?, '',?,?,?,?,1,?,?,?,?)`,
		r.RouteKey, r.DisplayName, r.BotAccountID, r.DestinationID, r.InboundEnabled, r.InboundBackendURL, backendCiphertext, r.OutboundEnabled, callers, r.Enabled, r.Status, r.LastValidatedAt, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	_, err = exec.Exec(`INSERT INTO gateway_route_revisions(route_id,revision,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,validated_at,created_at) VALUES(?,?,?,?,?,?,?, '',?,?,?,?,?,?,?)`,
		id, 1, r.DisplayName, r.BotAccountID, r.DestinationID, r.InboundEnabled, r.InboundBackendURL, backendCiphertext, r.OutboundEnabled, callers, r.Enabled, r.Status, r.LastValidatedAt, now)
	return id, err
}

func (s *Store) UpdateBusinessRoute(routeKey string, r models.BusinessRoute) error {
	existing, err := s.GetBusinessRoute(routeKey)
	if err != nil {
		return err
	}
	if r.RouteKey != "" && r.RouteKey != existing.RouteKey {
		return ErrRouteKeyImmutable
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nextRevision := existing.Revision + 1
	now := nowRFC3339()
	callers := encodeCallers(r.AllowedCallers)
	backendCiphertext, err := s.sealSecret(r.InboundBackendToken)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE gateway_business_routes SET display_name=?,bot_account_id=?,destination_id=?,inbound_enabled=?,inbound_backend_url=?,inbound_backend_token='',inbound_backend_token_ciphertext=?,outbound_enabled=?,allowed_callers=?,enabled=?,revision=?,status=?,last_validated_at=?,updated_at=? WHERE route_key=? AND revision=?`,
		r.DisplayName, r.BotAccountID, r.DestinationID, r.InboundEnabled, r.InboundBackendURL, backendCiphertext,
		r.OutboundEnabled, callers, r.Enabled, nextRevision, r.Status, r.LastValidatedAt, now, routeKey, existing.Revision)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("business route was modified concurrently")
	}
	_, err = tx.Exec(`INSERT INTO gateway_route_revisions(route_id,revision,display_name,bot_account_id,destination_id,inbound_enabled,inbound_backend_url,inbound_backend_token,inbound_backend_token_ciphertext,outbound_enabled,allowed_callers,enabled,status,validated_at,created_at) VALUES(?,?,?,?,?,?,?, '',?,?,?,?,?,?,?)`,
		existing.ID, nextRevision, r.DisplayName, r.BotAccountID, r.DestinationID, r.InboundEnabled, r.InboundBackendURL, backendCiphertext, r.OutboundEnabled, callers, r.Enabled, r.Status, r.LastValidatedAt, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetBusinessRoute(routeKey string) (*models.BusinessRoute, error) {
	return s.scanBusinessRoute(s.db.QueryRow(businessRouteSelect+` WHERE r.route_key=?`, routeKey))
}

func (s *Store) GetBusinessRoutes() ([]models.BusinessRoute, error) {
	rows, err := s.db.Query(businessRouteSelect + ` ORDER BY r.route_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.BusinessRoute{}
	for rows.Next() {
		r, err := s.scanBusinessRoute(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *r)
	}
	return result, rows.Err()
}

// CreateBusinessRouteSetup commits the three validated objects together.
func (s *Store) CreateBusinessRouteSetup(a models.BotAccount, d models.TelegramDestination, r models.BusinessRoute) (*models.BusinessRoute, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	sealedToken, err := s.sealSecret(a.Token)
	if err != nil {
		return nil, err
	}
	accountRes, err := tx.Exec(`INSERT INTO gateway_bot_accounts(name,username,token,token_ciphertext,token_fingerprint,revision,created_at,updated_at) VALUES(?,?, '',?,?,1,?,?)`, a.Name, a.Username, sealedToken, s.secretFingerprint(a.Token), now, now)
	if err != nil {
		return nil, err
	}
	accountID, _ := accountRes.LastInsertId()
	destRes, err := tx.Exec(`INSERT INTO gateway_telegram_destinations(name,bot_account_id,chat_id,chat_title,status,validated_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, d.Name, accountID, d.ChatID, d.ChatTitle, "active", d.ValidatedAt, now, now)
	if err != nil {
		return nil, err
	}
	destinationID, _ := destRes.LastInsertId()
	r.BotAccountID, r.DestinationID = accountID, destinationID
	_, err = insertBusinessRoute(s, tx, r)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBusinessRoute(r.RouteKey)
}

func (s *Store) ValidateBusinessRouteReferences(botAccountID, destinationID int64) error {
	var destinationBotID int64
	if err := s.db.QueryRow(`SELECT bot_account_id FROM gateway_telegram_destinations WHERE id=?`, destinationID).Scan(&destinationBotID); err != nil {
		return err
	}
	if destinationBotID != botAccountID {
		return fmt.Errorf("destination does not belong to bot account")
	}
	return nil
}
