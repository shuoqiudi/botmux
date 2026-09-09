package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

type storedAdapterExecution struct {
	*models.AdapterExecution
	ReplyPayload json.RawMessage `json:"reply_payload,omitempty"`
}

func (s *Store) migrateAdapter() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS gateway_adapter_executions (
        delivery_id TEXT PRIMARY KEY REFERENCES gateway_inbound_deliveries(delivery_id),
        state_json TEXT NOT NULL, job_fingerprint TEXT NOT NULL DEFAULT '',
        lease_owner TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0
    );
    CREATE TABLE IF NOT EXISTS gateway_adapter_replies (
        delivery_id TEXT PRIMARY KEY REFERENCES gateway_deliveries(id),
        inbound_id TEXT NOT NULL REFERENCES gateway_inbound_deliveries(delivery_id),
        phase TEXT NOT NULL, UNIQUE(inbound_id,phase)
    );
    INSERT OR IGNORE INTO gateway_workloads(id,name,status,created_at,updated_at)
        VALUES(-1234,'Embedded Gateway Adapter','active','','');`)
	return err
}

// ClaimAdapterExecution fences concurrent Stream consumers and restart reclaim.
// A persisted triggering state is never returned as a new trigger opportunity.
func (s *Store) ClaimAdapterExecution(ctx context.Context, id, owner, fingerprint string, lease time.Duration) (*models.AdapterExecution, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	initial := models.AdapterExecution{DeliveryID: id, Stage: "new", StartedAt: now, StageStartedAt: now}
	raw, _ := json.Marshal(initial)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO gateway_adapter_executions(delivery_id,state_json,job_fingerprint) VALUES(?,?,?)`, id, string(raw), fingerprint); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_adapter_executions SET lease_owner=?,lease_until=? WHERE delivery_id=? AND lease_until<=?`, owner, now+lease.Milliseconds(), id, now)
	if err != nil {
		return nil, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return nil, sql.ErrNoRows
	}
	var state models.AdapterExecution
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT state_json,job_fingerprint FROM gateway_adapter_executions WHERE delivery_id=?`, id).Scan(&encoded, &state.JobFingerprint); err != nil {
		return nil, err
	}
	stored := storedAdapterExecution{AdapterExecution: &state}
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
		return nil, err
	}
	// Pre-output versions persisted the result reply only in the outbound table.
	// Preserve those exact bytes on upgrade instead of generating a new payload
	// for an already-enqueued (possibly already-sent) phase.
	if state.Stage == "terminal" && len(stored.ReplyPayload) == 0 {
		err := tx.QueryRowContext(ctx, `SELECT p.payload_json FROM gateway_adapter_replies r
            JOIN gateway_delivery_payloads p ON p.delivery_id=r.delivery_id
            WHERE r.inbound_id=? AND r.phase='result'`, id).Scan(&stored.ReplyPayload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	state.ReplyPayload = stored.ReplyPayload
	return &state, nil
}

func (s *Store) SaveAdapterExecution(ctx context.Context, state *models.AdapterExecution, owner string, release bool) error {
	raw, err := json.Marshal(storedAdapterExecution{AdapterExecution: state, ReplyPayload: state.ReplyPayload})
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE gateway_adapter_executions SET state_json=?,lease_until=CASE WHEN ? THEN 0 ELSE lease_until END
        WHERE delivery_id=? AND lease_owner=? AND lease_until>?`, string(raw), release, state.DeliveryID, owner, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("adapter_lease_lost")
	}
	return nil
}

func (s *Store) GetAdapterExecution(ctx context.Context, id string) (*models.AdapterExecution, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT state_json FROM gateway_adapter_executions WHERE delivery_id=?`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var state models.AdapterExecution
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// CreateAdapterReply shares the durable outbound lifecycle without provisioning
// a public caller credential. The link to the inbound Delivery pins the chat.
func (s *Store) CreateAdapterReply(ctx context.Context, inboundID, phase string, payload []byte) (*models.GatewayDelivery, error) {
	if phase != "ack" && phase != "result" {
		return nil, errors.New("invalid adapter reply phase")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id := fmt.Sprintf("adapter-%x", sha256.Sum256([]byte(inboundID+":"+phase)))
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	now := nowRFC3339()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO gateway_deliveries
        (id,route_id,route_revision,workload_id,direction,action,status,payload_hash,idempotency_key,created_at,updated_at)
        SELECT ?,route_id,route_revision,-1234,'outbound','messages.send','pending_enqueue',?,?,?,?
        FROM gateway_inbound_deliveries WHERE delivery_id=?`, id, hash, inboundID+":"+phase, now, now, inboundID)
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	if n == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_delivery_payloads(delivery_id,payload_json) VALUES(?,?)`, id, payload); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_adapter_replies(delivery_id,inbound_id,phase) VALUES(?,?,?)`, id, inboundID, phase); err != nil {
			return nil, err
		}
	}
	var existingHash string
	if err := tx.QueryRowContext(ctx, `SELECT payload_hash FROM gateway_deliveries WHERE id=?`, id).Scan(&existingHash); err != nil {
		return nil, err
	}
	if existingHash != hash {
		return nil, ErrGatewayIdempotencyConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	delivery, _, err := s.GetGatewayDeliveryForWorker(ctx, id)
	return delivery, err
}

// ResolveAdapterReplyTarget returns nil for ordinary public outbound calls.
// Credentials may rotate, but the original Bot identity and Chat stay pinned.
func (s *Store) ResolveAdapterReplyTarget(ctx context.Context, id string) (*models.GatewayOutboundTarget, error) {
	var target models.GatewayOutboundTarget
	var ciphertext string
	err := s.db.QueryRowContext(ctx, `SELECT r.route_key,a.id,r.enabled,r.outbound_enabled,a.token_ciphertext,
        COALESCE(json_extract(d.raw_update,'$.message.chat.id'),json_extract(d.raw_update,'$.channel_post.chat.id'),0)
        FROM gateway_adapter_replies p JOIN gateway_inbound_deliveries d ON d.delivery_id=p.inbound_id
        JOIN gateway_business_routes r ON r.id=d.route_id JOIN gateway_bot_accounts a ON a.id=d.bot_account_id
        WHERE p.delivery_id=?`, id).Scan(&target.RouteKey, &target.BotAccountID, &target.Enabled, &target.Outbound, &ciphertext, &target.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	target.Token, err = s.openSecret(ciphertext)
	return &target, err
}
