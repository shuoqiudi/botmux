package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/skrashevich/botmux/internal/models"
)

var ErrDuplicateSubscription = errors.New("this Bot and Chat already subscribe to the service")
var ErrInvalidSubscriptionTarget = errors.New("an active Telegram destination with a valid Bot is required")

// Service deliveries have no Business Route. Rebuild the two legacy tables
// without losing their columns, indexes, payload references or delivery history.
func (s *Store) migrateServiceSubscriptions() error {
	for _, table := range []string{"gateway_deliveries", "gateway_dlq"} {
		if err := s.makeDeliveryRouteOptional(table); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(s.db, "gateway_notification_services", "revision", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	_, err := s.db.Exec(`
 CREATE TABLE IF NOT EXISTS gateway_service_subscriptions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 service_id INTEGER NOT NULL REFERENCES gateway_notification_services(id),
 destination_id INTEGER NOT NULL REFERENCES gateway_telegram_destinations(id),
 active INTEGER NOT NULL DEFAULT 1,
 created_at TEXT NOT NULL,
 cancelled_at TEXT NOT NULL DEFAULT ''
 );
 CREATE UNIQUE INDEX IF NOT EXISTS idx_service_active_destination ON gateway_service_subscriptions(service_id,destination_id) WHERE active=1;
 CREATE TABLE IF NOT EXISTS gateway_service_recipients (
 delivery_id TEXT PRIMARY KEY REFERENCES gateway_deliveries(id),
 notification_id TEXT NOT NULL REFERENCES gateway_service_notifications(id),
 subscription_id INTEGER NOT NULL REFERENCES gateway_service_subscriptions(id),
 bot_account_id INTEGER NOT NULL REFERENCES gateway_bot_accounts(id),
 telegram_bot_id INTEGER NOT NULL,
 chat_id INTEGER NOT NULL,
 UNIQUE(notification_id,subscription_id)
 );
 CREATE INDEX IF NOT EXISTS idx_service_recipient_notification ON gateway_service_recipients(notification_id);
 CREATE TABLE IF NOT EXISTS gateway_service_outbox (
 delivery_id TEXT PRIMARY KEY REFERENCES gateway_deliveries(id),
 replay_generation INTEGER NOT NULL DEFAULT 0
 );`)
	if err != nil {
		return err
	}
	return addColumnIfMissing(s.db, "gateway_service_outbox", "replay_generation", "INTEGER NOT NULL DEFAULT 0")
}

func (s *Store) makeDeliveryRouteOptional(table string) error {
	var required int
	if err := s.db.QueryRow(`SELECT "notnull" FROM pragma_table_info(?) WHERE name='route_id'`, table).Scan(&required); err != nil {
		return err
	}
	if required == 0 {
		return nil
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	var foreignKeys int
	if err := conn.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys=`+strconv.Itoa(foreignKeys))
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ddl string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT sql FROM sqlite_master WHERE tbl_name=? AND type IN ('index','trigger') AND sql IS NOT NULL`, table)
	if err != nil {
		return err
	}
	var definitions []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			rows.Close()
			return err
		}
		definitions = append(definitions, definition)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	ddl = strings.Replace(ddl, table, table+"_optional_route", 1)
	ddl = strings.Replace(ddl, "route_id INTEGER NOT NULL", "route_id INTEGER", 1)
	if _, err := tx.Exec(ddl); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ` + table + `_optional_route SELECT * FROM ` + table); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE ` + table); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE ` + table + `_optional_route RENAME TO ` + table); err != nil {
		return err
	}
	for _, definition := range definitions {
		if _, err := tx.Exec(definition); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Telegram tokens encode the immutable numeric Bot identity before the colon.
// Tokens are validated by the management HTTP API before account installation.
func telegramBotID(token string) (int64, error) {
	prefix, secret, ok := strings.Cut(token, ":")
	id, err := strconv.ParseInt(prefix, 10, 64)
	if !ok || secret == "" || err != nil || id <= 0 {
		return 0, ErrInvalidSubscriptionTarget
	}
	return id, nil
}

type serviceRecipient struct{ subscriptionID, accountID, botID, chatID int64 }

func (s *Store) serviceRecipients(ctx context.Context, tx *sql.Tx, serviceID int64) ([]serviceRecipient, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id,d.bot_account_id,a.token_ciphertext,d.chat_id,d.status FROM gateway_service_subscriptions p JOIN gateway_telegram_destinations d ON d.id=p.destination_id JOIN gateway_bot_accounts a ON a.id=d.bot_account_id WHERE p.service_id=? AND p.active=1 ORDER BY p.id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recipients := []serviceRecipient{}
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var recipient serviceRecipient
		var ciphertext, status string
		if err := rows.Scan(&recipient.subscriptionID, &recipient.accountID, &ciphertext, &recipient.chatID, &status); err != nil {
			return nil, err
		}
		token, err := s.openSecret(ciphertext)
		if err != nil {
			return nil, err
		}
		recipient.botID, err = telegramBotID(token)
		if err != nil || status != "active" || recipient.chatID == 0 {
			return nil, ErrInvalidSubscriptionTarget
		}
		key := [2]int64{recipient.botID, recipient.chatID}
		if seen[key] {
			return nil, ErrDuplicateSubscription
		}
		seen[key] = true
		recipients = append(recipients, recipient)
	}
	return recipients, rows.Err()
}

func (s *Store) AddServiceSubscription(ctx context.Context, serviceID, destinationID, expected int64, actor string) (int64, error) {
	if expected <= 0 {
		return 0, ErrExpectedRevisionRequired
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM gateway_notification_services WHERE id=?`, serviceID).Scan(&revision); err != nil {
		return 0, err
	}
	if expected != revision {
		return 0, ErrRevisionConflict
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT status='active' FROM gateway_telegram_destinations WHERE id=?`, destinationID).Scan(&active); err != nil || !active {
		return 0, ErrInvalidSubscriptionTarget
	}
	var duplicate int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_service_subscriptions WHERE service_id=? AND destination_id=? AND active=1`, serviceID, destinationID).Scan(&duplicate); err != nil {
		return 0, err
	}
	if duplicate > 0 {
		return 0, ErrDuplicateSubscription
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO gateway_service_subscriptions(service_id,destination_id,created_at) VALUES(?,?,?)`, serviceID, destinationID, nowRFC3339())
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := s.serviceRecipients(ctx, tx, serviceID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_notification_services SET revision=revision+1 WHERE id=?`, serviceID); err != nil {
		return 0, err
	}
	if err := appendGatewayAudit(tx, 0, revision+1, "admin", actor, "subscription.create", map[string]any{"service_id": serviceID, "subscription_id": id, "destination_id": destinationID}); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) CancelServiceSubscription(ctx context.Context, serviceID, id, expected int64, actor string) error {
	if expected <= 0 {
		return ErrExpectedRevisionRequired
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM gateway_notification_services WHERE id=?`, serviceID).Scan(&revision); err != nil {
		return err
	}
	if revision != expected {
		return ErrRevisionConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_service_subscriptions SET active=0,cancelled_at=? WHERE id=? AND service_id=? AND active=1`, nowRFC3339(), id, serviceID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_notification_services SET revision=revision+1 WHERE id=?`, serviceID); err != nil {
		return err
	}
	if err := appendGatewayAudit(tx, 0, revision+1, "admin", actor, "subscription.cancel", map[string]any{"service_id": serviceID, "subscription_id": id}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListServiceSubscriptions(ctx context.Context, id int64) ([]models.ServiceSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.destination_id,d.bot_account_id,a.name,d.name,d.chat_title,p.active FROM gateway_service_subscriptions p JOIN gateway_telegram_destinations d ON d.id=p.destination_id JOIN gateway_bot_accounts a ON a.id=d.bot_account_id WHERE p.service_id=? AND p.active=1 ORDER BY p.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []models.ServiceSubscription{}
	for rows.Next() {
		var p models.ServiceSubscription
		if err := rows.Scan(&p.ID, &p.DestinationID, &p.BotAccountID, &p.BotName, &p.DestinationName, &p.ChatTitle, &p.Active); err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

func createServiceDeliveries(ctx context.Context, tx *sql.Tx, n *models.ServiceNotification, workloadID int64, hash string, recipients []serviceRecipient) error {
	payload, err := json.Marshal(map[string]any{"kind": "message", "message": map[string]string{"text": n.Text, "parse_mode": n.ParseMode}})
	if err != nil {
		return err
	}
	for _, recipient := range recipients {
		id := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_deliveries(id,route_id,route_revision,workload_id,direction,action,status,payload_hash,idempotency_key,created_at,updated_at) VALUES(?,NULL,0,?,'outbound','messages.send','pending_enqueue',?,?,?,?)`, id, workloadID, hash, id, n.ReceivedAt, n.ReceivedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_delivery_payloads(delivery_id,payload_json) VALUES(?,?)`, id, payload); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_service_recipients(delivery_id,notification_id,subscription_id,bot_account_id,telegram_bot_id,chat_id) VALUES(?,?,?,?,?,?)`, id, n.ID, recipient.subscriptionID, recipient.accountID, recipient.botID, recipient.chatID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_service_outbox(delivery_id) VALUES(?)`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PendingServiceEnqueues(ctx context.Context) ([]models.ServiceEnqueue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT delivery_id,replay_generation FROM gateway_service_outbox ORDER BY rowid LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []models.ServiceEnqueue
	for rows.Next() {
		var id models.ServiceEnqueue
		if err := rows.Scan(&id.DeliveryID, &id.ReplayGeneration); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) CompleteServiceEnqueue(ctx context.Context, item models.ServiceEnqueue) error {
	id := item.DeliveryID
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT replay_generation FROM gateway_service_outbox WHERE delivery_id=?`, id).Scan(&generation); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if generation != item.ReplayGeneration {
		return nil
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status=CASE WHEN status IN ('pending_enqueue','replay_pending') THEN 'accepted' ELSE status END,accepted_at=CASE WHEN accepted_at='' THEN ? ELSE accepted_at END,updated_at=? WHERE id=?`, now, now, id); err != nil {
		return err
	}
	if item.ReplayGeneration > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE gateway_dlq SET state='replayed',acted_at=? WHERE delivery_id=? AND state='replay_pending' AND replay_count=?`, now, id, item.ReplayGeneration); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_service_outbox WHERE delivery_id=? AND replay_generation=?`, id, item.ReplayGeneration); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrServiceBotIdentityChanged = errors.New("service_bot_identity_changed")

func (s *Store) ResolveServiceDeliveryTarget(ctx context.Context, id string) (*models.GatewayOutboundTarget, error) {
	var target models.GatewayOutboundTarget
	var ciphertext string
	var botID int64
	err := s.db.QueryRowContext(ctx, `SELECT p.bot_account_id,p.telegram_bot_id,p.chat_id,a.token_ciphertext FROM gateway_service_recipients p JOIN gateway_bot_accounts a ON a.id=p.bot_account_id WHERE p.delivery_id=?`, id).Scan(&target.BotAccountID, &botID, &target.ChatID, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	target.Token, err = s.openSecret(ciphertext)
	if err != nil {
		return nil, err
	}
	current, err := telegramBotID(target.Token)
	if err != nil || current != botID {
		return nil, ErrServiceBotIdentityChanged
	}
	target.Enabled = true
	target.Outbound = true
	return &target, nil
}

// Account and destination changes share the acceptance lock and transaction, so
// they cannot introduce duplicate actual recipients between snapshot reads.
func (s *Store) validateServiceRecipients(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT DISTINCT service_id FROM gateway_service_subscriptions WHERE active=1`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.serviceRecipients(context.Background(), tx, id); err != nil {
			return err
		}
	}
	return nil
}
