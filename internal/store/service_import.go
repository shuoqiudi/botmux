package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/skrashevich/botmux/internal/models"
)

var ErrServiceImportConflict = errors.New("service import conflicts with recorded migration or current recipients")

// ImportNotificationServices commits the entire manifest and its receipt together.
// It never creates a notification or queue work and never changes existing recipients.
func (s *Store) ImportNotificationServices(ctx context.Context, input models.ServiceImport, actor string) ([]int64, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var oldHash, receipt string
	err = tx.QueryRowContext(ctx, `SELECT manifest_hash,service_ids FROM gateway_service_imports WHERE migration_id=?`, input.MigrationID).Scan(&oldHash, &receipt)
	if err == nil {
		if oldHash != hash {
			return nil, ErrServiceImportConflict
		}
		var ids []int64
		err = json.Unmarshal([]byte(receipt), &ids)
		return ids, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	ids := []int64{}
	for _, entry := range input.Services {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT status='active' FROM gateway_workloads WHERE id=?`, entry.WorkloadID).Scan(&active); err != nil {
			return nil, err
		}
		if !active {
			return nil, ErrServiceImportConflict
		}
		// Existing services must have precisely the imported recipients. In particular,
		// a new migration ID cannot resurrect a cancelled subscription.
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM gateway_notification_services WHERE workload_id=? AND fingerprint=?`, entry.WorkloadID, entry.Fingerprint).Scan(&id)
		existing := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if !existing {
			result, err := tx.ExecContext(ctx, `INSERT INTO gateway_notification_services(workload_id,fingerprint,display_name,last_received_at) VALUES(?,?,?,'')`, entry.WorkloadID, entry.Fingerprint, entry.DisplayName)
			if err != nil {
				return nil, err
			}
			id, err = result.LastInsertId()
			if err != nil {
				return nil, err
			}
		}
		for _, destination := range entry.Destinations {
			var account, chat int64
			var status, ciphertext string
			err := tx.QueryRowContext(ctx, `SELECT d.bot_account_id,d.chat_id,d.status,a.token_ciphertext FROM gateway_telegram_destinations d JOIN gateway_bot_accounts a ON a.id=d.bot_account_id WHERE d.id=?`, destination.DestinationID).Scan(&account, &chat, &status, &ciphertext)
			if err != nil {
				return nil, err
			}
			token, err := s.openSecret(ciphertext)
			if err != nil {
				return nil, err
			}
			botID, err := telegramBotID(token)
			if err != nil || status != "active" || account != destination.BotAccountID || chat != destination.ChatID || botID != destination.TelegramBotID {
				return nil, ErrServiceImportConflict
			}
			if existing {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_service_subscriptions WHERE service_id=? AND destination_id=? AND active=1`, id, destination.DestinationID).Scan(&count); err != nil {
					return nil, err
				}
				if count != 1 {
					return nil, ErrServiceImportConflict
				}
			} else {
				if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_service_subscriptions(service_id,destination_id,created_at) VALUES(?,?,?)`, id, destination.DestinationID, nowRFC3339()); err != nil {
					return nil, err
				}
			}
		}
		recipients, err := s.serviceRecipients(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if len(recipients) != len(entry.Destinations) {
			return nil, ErrServiceImportConflict
		}
		ids = append(ids, id)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_service_imports(migration_id,manifest_hash,service_ids,created_at) VALUES(?,?,?,?)`, input.MigrationID, hash, string(encoded), nowRFC3339()); err != nil {
		return nil, err
	}
	if err := appendGatewayAudit(tx, 0, 0, "admin", actor, "services.import", map[string]any{"migration_id": input.MigrationID, "manifest_hash": hash, "service_ids": ids}); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}
