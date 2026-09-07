package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

var (
	ErrGatewayDLQNotFound  = errors.New("Gateway DLQ item not found")
	ErrGatewayDLQDiscarded = errors.New("Gateway DLQ item was discarded")
	ErrGatewayDLQReplayed  = errors.New("Gateway DLQ item was already replayed")
)

func (s *Store) GetGatewayRouteMetrics(ctx context.Context, routeKey string, now time.Time) (*models.GatewayRouteMetrics, error) {
	var routeID int64
	var tokenConfigured bool
	var validatedAt, destinationStatus, destinationValidated string
	result := &models.GatewayRouteMetrics{RouteKey: routeKey}
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.revision,(a.token_ciphertext<>''),r.last_validated_at,
		d.status,d.validated_at,COALESCE((SELECT b.id FROM bots b WHERE b.token_fingerprint=a.token_fingerprint LIMIT 1),0)
		FROM gateway_business_routes r
		JOIN gateway_bot_accounts a ON a.id=r.bot_account_id
		JOIN gateway_telegram_destinations d ON d.id=r.destination_id
		WHERE r.route_key=?`, routeKey).Scan(&routeID, &result.Revision, &tokenConfigured, &validatedAt,
		&destinationStatus, &destinationValidated, &result.NativeBotID)
	if err != nil {
		return nil, err
	}
	result.Components.BotAuthentication = componentFromValidation(tokenConfigured, validatedAt, "credential_not_configured")
	result.Components.DestinationValidation = componentFromValidation(destinationStatus == "active", destinationValidated, "destination_not_validated")

	rows, err := s.db.QueryContext(ctx, `SELECT status,attempt_count,created_at,accepted_at,completed_at FROM gateway_deliveries WHERE route_id=?
		UNION ALL
		SELECT status,attempt_count,created_at,created_at,CASE WHEN status='SUCCEEDED' THEN updated_at ELSE '' END
		FROM gateway_inbound_deliveries WHERE route_id=?`, routeID, routeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var oldest time.Time
	var latencyTotal, latencyMax int64
	var completedCount int64
	for rows.Next() {
		var status, createdAt, acceptedAt, completedAt string
		var attempts int
		if err := rows.Scan(&status, &attempts, &createdAt, &acceptedAt, &completedAt); err != nil {
			return nil, err
		}
		result.AttemptCount += attempts
		normalized := strings.ToLower(status)
		switch normalized {
		case "accepted", "pending", "pending_enqueue", "processing", "replay_pending":
			result.Counts.Accepted++
		case "succeeded":
			result.Counts.Succeeded++
		case "retrying":
			result.Counts.Retrying++
		case "reconciling":
			result.Counts.Reconciling++
		case "dead-lettered", "dlq":
			result.Counts.DeadLettered++
		}
		if normalized == "accepted" || normalized == "pending" || normalized == "pending_enqueue" || normalized == "processing" || normalized == "retrying" || normalized == "replay_pending" {
			result.QueueDepth++
			if created, parseErr := parseGatewayTime(createdAt); parseErr == nil && (oldest.IsZero() || created.Before(oldest)) {
				oldest = created
			}
		}
		start, startErr := parseGatewayTime(acceptedAt)
		end, endErr := parseGatewayTime(completedAt)
		if startErr == nil && endErr == nil && !end.Before(start) {
			latency := end.Sub(start).Milliseconds()
			latencyTotal += latency
			completedCount++
			if latency > latencyMax {
				latencyMax = latency
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !oldest.IsZero() && now.After(oldest) {
		result.OldestPendingAgeMS = now.Sub(oldest).Milliseconds()
	}
	if completedCount > 0 {
		result.AverageEndToEndMS = latencyTotal / completedCount
		result.MaximumEndToEndMS = latencyMax
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM gateway_dlq WHERE route_id=? AND state IN ('active','replay_pending'))+
		(SELECT COUNT(*) FROM gateway_inbound_dlq WHERE route_id=? AND state IN ('active','replay_pending'))`, routeID, routeID).Scan(&result.DLQCount); err != nil {
		return nil, err
	}

	var backendStatus, backendChecked string
	err = s.db.QueryRowContext(ctx, `SELECT status,updated_at FROM gateway_inbound_deliveries WHERE route_id=? ORDER BY updated_at DESC LIMIT 1`, routeID).
		Scan(&backendStatus, &backendChecked)
	if errors.Is(err, sql.ErrNoRows) {
		result.Components.BackendHealth = models.GatewayComponentHealth{Status: "unknown", Detail: "no_delivery_observation"}
	} else if err != nil {
		return nil, err
	} else {
		status := "degraded"
		if strings.EqualFold(backendStatus, models.InboundSucceeded) {
			status = "healthy"
		} else if strings.EqualFold(backendStatus, models.InboundPending) {
			status = "unknown"
		}
		result.Components.BackendHealth = models.GatewayComponentHealth{Status: status, Detail: strings.ToLower(backendStatus), CheckedAt: backendChecked}
	}
	return result, nil
}

func componentFromValidation(ok bool, checkedAt, detail string) models.GatewayComponentHealth {
	if !ok {
		return models.GatewayComponentHealth{Status: "unhealthy", Detail: detail, CheckedAt: checkedAt}
	}
	if checkedAt == "" {
		return models.GatewayComponentHealth{Status: "unknown", Detail: "not_validated"}
	}
	return models.GatewayComponentHealth{Status: "healthy", CheckedAt: checkedAt}
}

func parseGatewayTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, value)
	}
	return parsed, err
}

func (s *Store) GetGatewayDeliveryDetail(ctx context.Context, routeKey, deliveryID string) (*models.GatewayDeliveryDetail, error) {
	var detail models.GatewayDeliveryDetail
	err := s.db.QueryRowContext(ctx, `SELECT d.id,r.route_key,d.route_revision,d.direction,d.action,d.status,d.attempt_count,
		d.safe_error_class,d.created_at,d.accepted_at,d.next_attempt_at,d.completed_at,d.updated_at
		FROM gateway_deliveries d JOIN gateway_business_routes r ON r.id=d.route_id WHERE r.route_key=? AND d.id=?`, routeKey, deliveryID).
		Scan(&detail.DeliveryID, &detail.RouteKey, &detail.RouteRevision, &detail.Direction, &detail.Action,
			&detail.Status, &detail.AttemptCount, &detail.ErrorClass, &detail.CreatedAt, &detail.AcceptedAt,
			&detail.NextAttemptAt, &detail.CompletedAt, &detail.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, `SELECT d.delivery_id,d.route_key,d.route_revision,'inbound','updates.deliver',d.status,d.attempt_count,
			d.last_error_class,d.created_at,d.created_at,d.next_attempt_at,CASE WHEN d.status='SUCCEEDED' THEN d.updated_at ELSE '' END,d.updated_at
			FROM gateway_inbound_deliveries d WHERE d.route_key=? AND d.delivery_id=?`, routeKey, deliveryID).
			Scan(&detail.DeliveryID, &detail.RouteKey, &detail.RouteRevision, &detail.Direction, &detail.Action,
				&detail.Status, &detail.AttemptCount, &detail.ErrorClass, &detail.CreatedAt, &detail.AcceptedAt,
				&detail.NextAttemptAt, &detail.CompletedAt, &detail.UpdatedAt)
	}
	if err != nil {
		return nil, err
	}
	detail.Attempts = []models.GatewayDeliveryAttempt{}
	if detail.Direction == "outbound" {
		rows, queryErr := s.db.QueryContext(ctx, `SELECT attempt_id,sequence,error_class,started_at,ended_at FROM gateway_delivery_attempts WHERE delivery_id=? ORDER BY sequence`, deliveryID)
		if queryErr != nil {
			return nil, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var attempt models.GatewayDeliveryAttempt
			if err := rows.Scan(&attempt.AttemptID, &attempt.Sequence, &attempt.ErrorClass, &attempt.StartedAt, &attempt.EndedAt); err != nil {
				return nil, err
			}
			attempt.Status = attemptStatus(attempt.ErrorClass, attempt.EndedAt)
			detail.Attempts = append(detail.Attempts, attempt)
		}
		return &detail, rows.Err()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT attempt_id,attempt_number,status,error_class,started_at,finished_at FROM gateway_inbound_attempts WHERE delivery_id=? ORDER BY attempt_number`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var attempt models.GatewayDeliveryAttempt
		if err := rows.Scan(&attempt.AttemptID, &attempt.Sequence, &attempt.Status, &attempt.ErrorClass, &attempt.StartedAt, &attempt.EndedAt); err != nil {
			return nil, err
		}
		attempt.Status = strings.ToLower(attempt.Status)
		detail.Attempts = append(detail.Attempts, attempt)
	}
	return &detail, rows.Err()
}

func attemptStatus(class, ended string) string {
	if ended == "" {
		return "processing"
	}
	if class == "" {
		return "succeeded"
	}
	return "failed"
}

const gatewayDLQUnion = `SELECT q.delivery_id,r.route_key,'outbound',q.state,q.error_class,q.attempt_count,q.replay_count,q.entered_at,q.acted_at
	FROM gateway_dlq q JOIN gateway_business_routes r ON r.id=q.route_id
	UNION ALL
	SELECT q.delivery_id,r.route_key,'inbound',q.state,q.error_class,q.attempt_count,q.replay_count,q.entered_at,q.acted_at
	FROM gateway_inbound_dlq q JOIN gateway_business_routes r ON r.id=q.route_id`

func scanGatewayDLQ(scanner interface{ Scan(...any) error }) (*models.GatewayDLQItem, error) {
	var item models.GatewayDLQItem
	err := scanner.Scan(&item.DeliveryID, &item.RouteKey, &item.Direction, &item.Status, &item.ErrorClass,
		&item.AttemptCount, &item.ReplayCount, &item.EnteredAt, &item.ActedAt)
	return &item, err
}

func (s *Store) ListGatewayDLQ(ctx context.Context, routeKey, state string, limit int) ([]models.GatewayDLQItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if state == "" {
		state = "open"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (`+gatewayDLQUnion+`) WHERE (?='' OR route_key=?)
		AND (?='all' OR (?='open' AND state IN ('active','replay_pending')) OR state=?) ORDER BY entered_at DESC LIMIT ?`,
		routeKey, routeKey, state, state, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []models.GatewayDLQItem{}
	for rows.Next() {
		item, err := scanGatewayDLQ(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (s *Store) PrepareGatewayDLQReplay(ctx context.Context, deliveryID, actorID string) (*models.GatewayDLQItem, bool, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	item, err := scanGatewayDLQ(tx.QueryRowContext(ctx, `SELECT * FROM (`+gatewayDLQUnion+`) WHERE delivery_id=?`, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrGatewayDLQNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if item.Status == "discarded" {
		return nil, false, ErrGatewayDLQDiscarded
	}
	if item.Status == "replayed" {
		return item, true, nil
	}
	if item.Status == "active" {
		item.ReplayCount++
		table := dlqTable(item.Direction)
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state='replay_pending',replay_count=?,acted_at='',acted_by=? WHERE delivery_id=? AND state='active'`, item.ReplayCount, actorID, deliveryID); err != nil {
			return nil, false, err
		}
		if item.Direction == "outbound" {
			_, err = tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='replay_pending',next_attempt_at='',completed_at='',updated_at=? WHERE id=?`, nowRFC3339(), deliveryID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET status='REPLAY_PENDING',next_attempt_at='',updated_at=? WHERE delivery_id=?`, nowRFC3339(), deliveryID)
		}
		if err != nil {
			return nil, false, err
		}
		var routeID, revision int64
		if err := tx.QueryRowContext(ctx, `SELECT id,revision FROM gateway_business_routes WHERE route_key=?`, item.RouteKey).Scan(&routeID, &revision); err != nil {
			return nil, false, err
		}
		if err := appendGatewayAudit(tx, routeID, revision, "operator", actorID, "dlq.replay", map[string]any{
			"delivery_id": deliveryID, "direction": item.Direction, "replay_count": item.ReplayCount,
		}); err != nil {
			return nil, false, err
		}
		item.Status = "replay_pending"
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return item, false, nil
}

func (s *Store) CompleteGatewayDLQReplay(ctx context.Context, deliveryID, direction, streamID, actorID string) error {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	table := dlqTable(direction)
	if table == "" {
		return errors.New("invalid delivery direction")
	}
	now := nowRFC3339()
	if direction == "outbound" {
		_, err = tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='accepted',safe_error_class='',accepted_at=?,updated_at=? WHERE id=? AND status='replay_pending'`, now, now, deliveryID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET status=?,stream_id=?,last_error_class='',updated_at=? WHERE delivery_id=? AND status='REPLAY_PENDING'`, models.InboundPending, streamID, now, deliveryID)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state='replayed',acted_at=?,acted_by=? WHERE delivery_id=? AND state='replay_pending'`, now, actorID, deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DiscardGatewayDLQ(ctx context.Context, deliveryID, actorID string) (*models.GatewayDLQItem, bool, error) {
	s.gatewayMu.Lock()
	defer s.gatewayMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	item, err := scanGatewayDLQ(tx.QueryRowContext(ctx, `SELECT * FROM (`+gatewayDLQUnion+`) WHERE delivery_id=?`, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrGatewayDLQNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if item.Status == "discarded" {
		return item, true, nil
	}
	if item.Status != "active" {
		return nil, false, ErrGatewayDLQReplayed
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `UPDATE `+dlqTable(item.Direction)+` SET state='discarded',acted_at=?,acted_by=? WHERE delivery_id=? AND state='active'`, now, actorID, deliveryID); err != nil {
		return nil, false, err
	}
	if item.Direction == "outbound" {
		_, err = tx.ExecContext(ctx, `UPDATE gateway_deliveries SET status='discarded',completed_at=?,updated_at=? WHERE id=? AND status='dead-lettered'`, now, now, deliveryID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE gateway_inbound_deliveries SET status='DISCARDED',updated_at=? WHERE delivery_id=? AND status=?`, now, deliveryID, models.InboundDLQ)
	}
	if err != nil {
		return nil, false, err
	}
	var routeID, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT id,revision FROM gateway_business_routes WHERE route_key=?`, item.RouteKey).Scan(&routeID, &revision); err != nil {
		return nil, false, err
	}
	if err := appendGatewayAudit(tx, routeID, revision, "operator", actorID, "dlq.discard", map[string]any{
		"delivery_id": deliveryID, "direction": item.Direction,
	}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	item.Status, item.ActedAt = "discarded", now
	return item, false, nil
}

func dlqTable(direction string) string {
	switch direction {
	case "outbound":
		return "gateway_dlq"
	case "inbound":
		return "gateway_inbound_dlq"
	default:
		return ""
	}
}

func (s *Store) GatewayRouteExists(ctx context.Context, routeKey string) error {
	var value int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM gateway_business_routes WHERE route_key=?`, routeKey).Scan(&value); err != nil {
		return fmt.Errorf("route lookup: %w", err)
	}
	return nil
}

func (s *Store) GetGatewayRouteProbeTarget(ctx context.Context, routeKey string) (*models.GatewayRouteProbeTarget, error) {
	var target models.GatewayRouteProbeTarget
	var tokenCiphertext, backendCiphertext string
	err := s.db.QueryRowContext(ctx, `SELECT a.token_ciphertext,d.chat_id,COALESCE(h.health_url,''),r.inbound_backend_token_ciphertext
		FROM gateway_business_routes r
		JOIN gateway_bot_accounts a ON a.id=r.bot_account_id
		JOIN gateway_telegram_destinations d ON d.id=r.destination_id
		LEFT JOIN gateway_backend_health_config h ON h.route_id=r.id
		WHERE r.route_key=?`, routeKey).Scan(&tokenCiphertext, &target.ChatID, &target.BackendHealthURL, &backendCiphertext)
	if err != nil {
		return nil, err
	}
	target.Token, err = s.openSecret(tokenCiphertext)
	if err != nil {
		return nil, err
	}
	target.BackendToken, err = s.openSecret(backendCiphertext)
	if err != nil {
		return nil, err
	}
	return &target, nil
}
