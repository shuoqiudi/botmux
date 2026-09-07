package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/skrashevich/botmux/internal/models"
)

func (s *Store) migrateInboundDeliveries() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS gateway_inbound_deliveries (
			delivery_id TEXT PRIMARY KEY,
			bot_id INTEGER NOT NULL,
			bot_account_id INTEGER NOT NULL DEFAULT 0,
			route_id INTEGER NOT NULL DEFAULT 0,
			route_revision INTEGER NOT NULL DEFAULT 0,
			route_key TEXT NOT NULL DEFAULT '',
			update_id INTEGER NOT NULL,
			callback_query_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			raw_update BLOB NOT NULL,
			payload_sha256 TEXT NOT NULL,
			stream_id TEXT NOT NULL DEFAULT '',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT NOT NULL DEFAULT '',
			last_error_class TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(bot_id, update_id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_gateway_inbound_callback_dedupe
			ON gateway_inbound_deliveries(bot_id, callback_query_id)
			WHERE callback_query_id <> '';
		CREATE INDEX IF NOT EXISTS idx_gateway_inbound_status
			ON gateway_inbound_deliveries(status, next_attempt_at);
		CREATE TABLE IF NOT EXISTS gateway_inbound_attempts (
			attempt_id TEXT PRIMARY KEY,
			delivery_id TEXT NOT NULL,
			attempt_number INTEGER NOT NULL,
			status TEXT NOT NULL,
			http_status INTEGER NOT NULL DEFAULT 0,
			error_class TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL DEFAULT '',
			UNIQUE(delivery_id, attempt_number),
			FOREIGN KEY(delivery_id) REFERENCES gateway_inbound_deliveries(delivery_id)
		);
		CREATE TABLE IF NOT EXISTS gateway_inbound_dlq (
			delivery_id TEXT PRIMARY KEY,
			route_id INTEGER NOT NULL,
			source_stream_id TEXT NOT NULL,
			error_class TEXT NOT NULL,
			attempt_count INTEGER NOT NULL,
			entered_at TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'active',
			replay_count INTEGER NOT NULL DEFAULT 0,
			acted_at TEXT NOT NULL DEFAULT '',
			acted_by TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(delivery_id) REFERENCES gateway_inbound_deliveries(delivery_id),
			FOREIGN KEY(route_id) REFERENCES gateway_business_routes(id)
		);
	`)
	return err
}

func (s *Store) scanInboundRoute(scanner interface{ Scan(...any) error }) (*models.InboundRoute, error) {
	var r models.InboundRoute
	var backendCiphertext string
	err := scanner.Scan(&r.ID, &r.Revision, &r.RouteKey, &r.BotAccountID, &r.Enabled,
		&r.InboundEnabled, &r.Status, &r.BackendURL, &backendCiphertext)
	if err != nil {
		return nil, err
	}
	r.BackendToken, err = s.openSecret(backendCiphertext)
	return &r, err
}

const inboundRouteSelect = `
	SELECT r.id,r.revision,r.route_key,r.bot_account_id,r.enabled,
		r.inbound_enabled,r.status,r.inbound_backend_url,r.inbound_backend_token_ciphertext
	FROM gateway_business_routes r
	JOIN gateway_bot_accounts a ON a.id=r.bot_account_id
	JOIN gateway_telegram_destinations d ON d.id=r.destination_id
	JOIN bots b ON b.id=? AND b.token_fingerprint=a.token_fingerprint
	WHERE d.bot_account_id=a.id AND d.chat_id=?
	ORDER BY (r.enabled=1 AND r.inbound_enabled=1 AND r.status='active') DESC,r.id
	LIMIT 1`

func (s *Store) ResolveInboundRoute(ctx context.Context, botID, chatID int64) (*models.InboundRoute, error) {
	return s.scanInboundRoute(s.db.QueryRowContext(ctx, inboundRouteSelect, botID, chatID))
}

func (s *Store) findInboundDelivery(scanner interface{ Scan(...any) error }) (*models.InboundDelivery, error) {
	var d models.InboundDelivery
	var backendCiphertext string
	err := scanner.Scan(&d.DeliveryID, &d.BotID, &d.BotAccountID, &d.RouteID, &d.RouteRevision,
		&d.RouteKey, &d.UpdateID, &d.CallbackQueryID, &d.Status, &d.RawUpdate, &d.StreamID,
		&d.AttemptCount, &d.NextAttemptAt, &d.LastErrorClass, &d.BackendURL,
		&backendCiphertext, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	d.BackendToken, err = s.openSecret(backendCiphertext)
	return &d, err
}

const inboundDeliverySelect = `
	SELECT d.delivery_id,d.bot_id,d.bot_account_id,d.route_id,d.route_revision,
		d.route_key,d.update_id,d.callback_query_id,d.status,d.raw_update,d.stream_id,
		d.attempt_count,d.next_attempt_at,d.last_error_class,
		COALESCE(rr.inbound_backend_url,''),COALESCE(rr.inbound_backend_token_ciphertext,''),
		d.created_at,d.updated_at
	FROM gateway_inbound_deliveries d
	LEFT JOIN gateway_route_revisions rr ON rr.route_id=d.route_id AND rr.revision=d.route_revision`

// PrepareInboundDelivery transactionally pins route resolution and stores the
// raw Update before any Redis append or Telegram offset advancement. Duplicate
// Update IDs and callback IDs return the original Delivery.
func (s *Store) PrepareInboundDelivery(ctx context.Context, botID, updateID int64, callbackID string, chatID int64, raw []byte) (*models.InboundDelivery, bool, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()

	if updateID <= 0 {
		return nil, false, errors.New("Telegram update_id must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	existingQuery := inboundDeliverySelect + ` WHERE d.bot_id=? AND (d.update_id=? OR (? <> '' AND d.callback_query_id=?)) LIMIT 1`
	if existing, err := s.findInboundDelivery(tx.QueryRowContext(ctx, existingQuery, botID, updateID, callbackID, callbackID)); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	route, routeErr := s.scanInboundRoute(tx.QueryRowContext(ctx, inboundRouteSelect, botID, chatID))
	status := models.InboundPending
	if errors.Is(routeErr, sql.ErrNoRows) {
		status = models.InboundRejectedUnknownRoute
		route = &models.InboundRoute{}
	} else if routeErr != nil {
		return nil, false, routeErr
	} else if !route.Enabled || !route.InboundEnabled || route.Status != models.GatewayRouteActive {
		status = models.InboundRejectedRouteDisabled
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	deliveryID := uuid.NewString()
	payloadHash := fmt.Sprintf("%x", sha256.Sum256(raw))
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_inbound_deliveries(
		delivery_id,bot_id,bot_account_id,route_id,route_revision,route_key,update_id,
		callback_query_id,status,raw_update,payload_sha256,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, deliveryID, botID, route.BotAccountID, route.ID,
		route.Revision, route.RouteKey, updateID, callbackID, status, raw, payloadHash, now, now)
	if err != nil {
		// A concurrent ingest may have won one of the two unique dedupe keys.
		_ = tx.Rollback()
		if duplicate, findErr := s.findInboundDelivery(s.db.QueryRowContext(ctx, existingQuery, botID, updateID, callbackID, callbackID)); findErr == nil {
			return duplicate, false, nil
		}
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	delivery, err := s.GetInboundDelivery(ctx, deliveryID)
	return delivery, true, err
}

func (s *Store) GetInboundDelivery(ctx context.Context, deliveryID string) (*models.InboundDelivery, error) {
	return s.findInboundDelivery(s.db.QueryRowContext(ctx, inboundDeliverySelect+` WHERE d.delivery_id=?`, deliveryID))
}

func (s *Store) GetInboundDeliveryByUpdate(ctx context.Context, botID, updateID int64) (*models.InboundDelivery, error) {
	return s.findInboundDelivery(s.db.QueryRowContext(ctx, inboundDeliverySelect+` WHERE d.bot_id=? AND d.update_id=?`, botID, updateID))
}

func (s *Store) SetInboundStreamID(ctx context.Context, deliveryID, streamID string) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()

	_, err := s.db.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET stream_id=?,updated_at=? WHERE delivery_id=? AND stream_id=''`,
		streamID, time.Now().UTC().Format(time.RFC3339Nano), deliveryID)
	return err
}

func (s *Store) BeginInboundAttempt(ctx context.Context, deliveryID, attemptID string) (int, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT attempt_count+1 FROM gateway_inbound_deliveries WHERE delivery_id=?`, deliveryID).Scan(&attempt); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET attempt_count=?,status=?,updated_at=? WHERE delivery_id=?`, attempt, models.InboundPending, now, deliveryID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_inbound_attempts(attempt_id,delivery_id,attempt_number,status,started_at) VALUES(?,?,?,?,?)`, attemptID, deliveryID, attempt, "STARTED", now); err != nil {
		return 0, err
	}
	return attempt, tx.Commit()
}

func (s *Store) CompleteInboundAttempt(ctx context.Context, deliveryID, attemptID string, httpStatus int) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_inbound_attempts SET status='SUCCEEDED',http_status=?,finished_at=? WHERE attempt_id=?`, httpStatus, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET status=?,next_attempt_at='',last_error_class='',updated_at=? WHERE delivery_id=?`, models.InboundSucceeded, now, deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailInboundAttempt(ctx context.Context, deliveryID, attemptID, class string, httpStatus int, next time.Time) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	nextValue := next.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_inbound_attempts SET status='FAILED',http_status=?,error_class=?,finished_at=? WHERE attempt_id=?`, httpStatus, class, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET status=?,next_attempt_at=?,last_error_class=?,updated_at=? WHERE delivery_id=?`, models.InboundRetrying, nextValue, class, now, deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkInboundDLQ(ctx context.Context, deliveryID, class string) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_inbound_dlq(delivery_id,route_id,source_stream_id,error_class,attempt_count,entered_at,state,acted_at,acted_by)
		SELECT delivery_id,route_id,stream_id,?,attempt_count,?,'active','','' FROM gateway_inbound_deliveries WHERE delivery_id=?
		ON CONFLICT(delivery_id) DO UPDATE SET source_stream_id=excluded.source_stream_id,error_class=excluded.error_class,
		attempt_count=excluded.attempt_count,entered_at=excluded.entered_at,state='active',acted_at='',acted_by=''`, class, now, deliveryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET status=?,next_attempt_at='',last_error_class=?,updated_at=? WHERE delivery_id=?`,
		models.InboundDLQ, class, now, deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}
