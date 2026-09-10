package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/skrashevich/botmux/internal/models"
)

func (s *Store) migrateServiceNotifications() error {
	for _, column := range []string{"service_publish", "service_query"} {
		if err := addColumnIfMissing(s.db, "gateway_workloads", column, "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`
 CREATE TABLE IF NOT EXISTS gateway_notification_services (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 workload_id INTEGER NOT NULL REFERENCES gateway_workloads(id),
 fingerprint TEXT NOT NULL,
 display_name TEXT NOT NULL,
 last_received_at TEXT NOT NULL,
 UNIQUE(workload_id,fingerprint)
 );
 CREATE TABLE IF NOT EXISTS gateway_service_notifications (
 id TEXT PRIMARY KEY,
 workload_id INTEGER NOT NULL REFERENCES gateway_workloads(id),
 service_id INTEGER NOT NULL REFERENCES gateway_notification_services(id),
 idempotency_key TEXT NOT NULL,
 payload_hash TEXT NOT NULL,
 text TEXT NOT NULL,
 parse_mode TEXT NOT NULL,
 status TEXT NOT NULL,
 subscription_count INTEGER NOT NULL,
 received_at TEXT NOT NULL,
 UNIQUE(workload_id,idempotency_key)
 );
 CREATE INDEX IF NOT EXISTS idx_service_notifications_history ON gateway_service_notifications(service_id,received_at DESC);
 `)
	if err != nil {
		return err
	}
	return s.migrateServiceSubscriptions()
}

func (s *Store) SetServicePermissions(id, expected int64, permissions models.ServicePermissions, actorID string) (*models.GatewayWorkloadAdmin, error) {
	if expected <= 0 {
		return nil, ErrExpectedRevisionRequired
	}
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
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
	if _, err := tx.Exec(`UPDATE gateway_workloads SET service_publish=?,service_query=?,revision=revision+1,updated_at=? WHERE id=?`, permissions.Publish, permissions.Query, nowRFC3339(), id); err != nil {
		return nil, err
	}
	if err := appendGatewayAudit(tx, 0, current+1, "admin", actorID, "service_permissions.replace", map[string]any{"workload_id": id, "publish": permissions.Publish, "query": permissions.Query}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetGatewayWorkloadAdmin(id)
}

func (s *Store) GetServicePermissions(ctx context.Context, id int64) (models.ServicePermissions, error) {
	var p models.ServicePermissions
	err := s.db.QueryRowContext(ctx, `SELECT service_publish,service_query FROM gateway_workloads WHERE id=? AND status='active'`, id).Scan(&p.Publish, &p.Query)
	return p, err
}

const serviceNotificationColumns = `id,service_id,status,subscription_count,received_at,text,parse_mode`

func scanServiceNotification(row interface{ Scan(...any) error }) (*models.ServiceNotification, error) {
	var n models.ServiceNotification
	if err := row.Scan(&n.ID, &n.ServiceID, &n.Status, &n.SubscriptionCount, &n.ReceivedAt, &n.Text, &n.ParseMode); err != nil {
		return nil, err
	}
	n.DeliverySummary = models.ServiceDeliverySummary{Status: n.Status, Total: n.SubscriptionCount}
	n.Deliveries = []models.ServiceNotificationDelivery{}
	return &n, nil
}

// AcceptServiceNotification commits the registration and receipt atomically. A
// replay is resolved before touching the service, including its display name.
func (s *Store) AcceptServiceNotification(ctx context.Context, workloadID int64, key, hash string, input models.ServiceNotificationRequest) (*models.ServiceNotification, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var originalHash string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM gateway_service_notifications WHERE workload_id=? AND idempotency_key=?`, workloadID, key).Scan(&originalHash)
	if err == nil {
		if originalHash != hash {
			return nil, ErrGatewayIdempotencyConflict
		}
		n, err := scanServiceNotification(tx.QueryRowContext(ctx, `SELECT `+serviceNotificationColumns+` FROM gateway_service_notifications WHERE workload_id=? AND idempotency_key=?`, workloadID, key))
		if err != nil {
			return nil, err
		}
		return n, loadServiceDeliveries(ctx, tx, n)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	name := input.Fingerprint
	if input.ServiceName != nil {
		name = *input.ServiceName
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_notification_services(workload_id,fingerprint,display_name,last_received_at) VALUES(?,?,?,?) ON CONFLICT(workload_id,fingerprint) DO UPDATE SET display_name=excluded.display_name,last_received_at=excluded.last_received_at`, workloadID, input.Fingerprint, name, now); err != nil {
		return nil, err
	}
	var serviceID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM gateway_notification_services WHERE workload_id=? AND fingerprint=?`, workloadID, input.Fingerprint).Scan(&serviceID); err != nil {
		return nil, err
	}
	recipients, err := s.serviceRecipients(ctx, tx, serviceID)
	if err != nil {
		return nil, err
	}
	status := "no_subscribers"
	if len(recipients) > 0 {
		status = "accepted"
	}
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_service_notifications(id,workload_id,service_id,idempotency_key,payload_hash,text,parse_mode,status,subscription_count,received_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, workloadID, serviceID, key, hash, input.Text, input.ParseMode, status, len(recipients), now); err != nil {
		return nil, err
	}
	n, err := scanServiceNotification(tx.QueryRowContext(ctx, `SELECT `+serviceNotificationColumns+` FROM gateway_service_notifications WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := createServiceDeliveries(ctx, tx, n, workloadID, hash, recipients); err != nil {
		return nil, err
	}
	if err := loadServiceDeliveries(ctx, tx, n); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return n, nil
}

func (s *Store) GetServiceNotification(ctx context.Context, workloadID int64, id string) (*models.ServiceNotification, error) {
	n, err := scanServiceNotification(s.db.QueryRowContext(ctx, `SELECT `+serviceNotificationColumns+` FROM gateway_service_notifications WHERE workload_id=? AND id=?`, workloadID, id))
	if err != nil {
		return nil, err
	}
	return n, loadServiceDeliveries(ctx, s.db, n)
}

const notificationServiceColumns = `s.id,s.workload_id,w.name,s.fingerprint,s.display_name,s.last_received_at,s.revision,(SELECT COUNT(*) FROM gateway_service_subscriptions p WHERE p.service_id=s.id AND p.active=1)`

func scanNotificationService(row interface{ Scan(...any) error }) (*models.NotificationService, error) {
	var s models.NotificationService
	err := row.Scan(&s.ID, &s.WorkloadID, &s.Source, &s.Fingerprint, &s.DisplayName, &s.LastReceivedAt, &s.Revision, &s.SubscriptionCount)
	return &s, err
}

func (s *Store) ListNotificationServices(ctx context.Context) ([]models.NotificationService, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+notificationServiceColumns+` FROM gateway_notification_services s JOIN gateway_workloads w ON w.id=s.workload_id ORDER BY s.last_received_at DESC,s.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []models.NotificationService{}
	for rows.Next() {
		item, err := scanNotificationService(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Store) GetNotificationService(ctx context.Context, id int64) (*models.NotificationService, error) {
	return scanNotificationService(s.db.QueryRowContext(ctx, `SELECT `+notificationServiceColumns+` FROM gateway_notification_services s JOIN gateway_workloads w ON w.id=s.workload_id WHERE s.id=?`, id))
}

// History is bounded; before is the last notification ID from the previous page.
func (s *Store) ServiceNotificationHistory(ctx context.Context, serviceID int64, before string) ([]models.ServiceNotification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+serviceNotificationColumns+` FROM gateway_service_notifications WHERE service_id=? AND (?='' OR rowid < (SELECT rowid FROM gateway_service_notifications WHERE id=? AND service_id=?)) ORDER BY rowid DESC LIMIT 100`, serviceID, before, before, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []models.ServiceNotification{}
	for rows.Next() {
		item, err := scanServiceNotification(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range items {
		if err := loadServiceDeliveries(ctx, s.db, &items[i]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func loadServiceDeliveries(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, n *models.ServiceNotification) error {
	rows, err := query.QueryContext(ctx, `SELECT d.id,d.status,d.safe_error_class,d.attempt_count FROM gateway_service_recipients p JOIN gateway_deliveries d ON d.id=p.delivery_id WHERE p.notification_id=? ORDER BY p.subscription_id`, n.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	n.Deliveries = []models.ServiceNotificationDelivery{}
	n.DeliverySummary = models.ServiceDeliverySummary{Status: "no_subscribers", Total: n.SubscriptionCount}
	if n.SubscriptionCount > 0 {
		n.DeliverySummary.Status = "pending"
	}
	for rows.Next() {
		var d models.ServiceNotificationDelivery
		if err := rows.Scan(&d.ID, &d.Status, &d.ErrorClass, &d.AttemptCount); err != nil {
			return err
		}
		n.Deliveries = append(n.Deliveries, d)
		if d.Status == "succeeded" {
			n.DeliverySummary.Succeeded++
		}
		if d.Status == "dead-lettered" || d.Status == "reconciling" || d.Status == "discarded" {
			n.DeliverySummary.Status = "failed"
		}
	}
	if n.SubscriptionCount > 0 && n.DeliverySummary.Succeeded == n.SubscriptionCount {
		n.DeliverySummary.Status = "succeeded"
	}
	return rows.Err()
}
