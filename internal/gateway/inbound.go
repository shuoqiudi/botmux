package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/skrashevich/botmux/internal/models"
)

type InboundStore interface {
	PrepareInboundDelivery(context.Context, int64, int64, string, int64, []byte) (*models.InboundDelivery, bool, error)
	GetInboundDelivery(context.Context, string) (*models.InboundDelivery, error)
	SetInboundStreamID(context.Context, string, string) error
	BeginInboundAttempt(context.Context, string, string) (int, error)
	CompleteInboundAttempt(context.Context, string, string, int) error
	FailInboundAttempt(context.Context, string, string, string, int, time.Time) error
	MarkInboundDLQ(context.Context, string, string) error
}

type StreamMessage struct {
	ID         string
	DeliveryID string
}

type InboundQueue interface {
	AppendUnique(context.Context, string) (string, error)
	EnsureGroup(context.Context) error
	ReadNew(context.Context, string, time.Duration, int64) ([]StreamMessage, error)
	ClaimStale(context.Context, string, time.Duration, int64) ([]StreamMessage, error)
	Ack(context.Context, string) error
	MoveToDLQAndAck(context.Context, StreamMessage, string, int) error
}

type IngestResult struct {
	DeliveryID string
	Status     string
	Duplicate  bool
}

// Inbound is the durable Telegram Update ingestion and backend delivery
// boundary. It deliberately knows nothing about native content-based routes.
type Inbound struct {
	store  InboundStore
	queue  InboundQueue
	client *http.Client
	config InboundConfig
}

type InboundConfig struct {
	Consumer     string
	MaxAttempts  int
	BaseRetry    time.Duration
	MaxRetry     time.Duration
	PollInterval time.Duration
	ClaimMinIdle time.Duration
	BatchSize    int64
}

func NewInbound(store InboundStore, queue InboundQueue, client *http.Client, config InboundConfig) *Inbound {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if config.Consumer == "" {
		config.Consumer = "botmux-" + uuid.NewString()
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 5
	}
	if config.BaseRetry <= 0 {
		config.BaseRetry = time.Second
	}
	if config.MaxRetry <= 0 {
		config.MaxRetry = 30 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.ClaimMinIdle <= 0 {
		config.ClaimMinIdle = time.Second
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 20
	}
	return &Inbound{store: store, queue: queue, client: client, config: config}
}

var chatEnvelopeKeys = [...]string{
	"message", "edited_message", "channel_post", "edited_channel_post",
	"business_message", "edited_business_message", "deleted_business_messages",
	"message_reaction", "message_reaction_count", "my_chat_member", "chat_member",
	"chat_join_request", "chat_boost", "removed_chat_boost",
}

// ExtractInboundIdentity examines only documented Update envelope locations.
// It never looks at text, commands, callback data, or other message content.
func ExtractInboundIdentity(update map[string]any) (updateID int64, callbackID string, chatID int64) {
	updateID = int64Value(update["update_id"])
	if callback, ok := object(update["callback_query"]); ok {
		callbackID, _ = callback["id"].(string)
		if message, ok := object(callback["message"]); ok {
			chatID = chatIDFromEnvelope(message)
		}
		return
	}
	for _, key := range chatEnvelopeKeys {
		if envelope, ok := object(update[key]); ok {
			return updateID, "", chatIDFromEnvelope(envelope)
		}
	}
	return
}

func object(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func chatIDFromEnvelope(envelope map[string]any) int64 {
	chat, ok := object(envelope["chat"])
	if !ok {
		return 0
	}
	return int64Value(chat["id"])
}

func int64Value(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

// IngestUpdate returns only after the raw Update is durable in SQLite and, for
// a matched active Route, durably appended to Redis. A nil error authorizes the
// polling owner to advance Telegram's offset.
func (i *Inbound) IngestUpdate(ctx context.Context, botID int64, update map[string]any) (IngestResult, error) {
	updateID, callbackID, chatID := ExtractInboundIdentity(update)
	raw, err := json.Marshal(update)
	if err != nil {
		return IngestResult{}, fmt.Errorf("marshal Telegram Update: %w", err)
	}
	delivery, created, err := i.store.PrepareInboundDelivery(ctx, botID, updateID, callbackID, chatID, raw)
	if err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{DeliveryID: delivery.DeliveryID, Status: delivery.Status, Duplicate: !created}
	if delivery.Status == models.InboundRejectedUnknownRoute || delivery.Status == models.InboundRejectedRouteDisabled ||
		delivery.Status == models.InboundSucceeded || delivery.Status == models.InboundDLQ {
		return result, nil
	}
	if delivery.StreamID != "" {
		return result, nil
	}
	streamID, err := i.queue.AppendUnique(ctx, delivery.DeliveryID)
	if err != nil {
		return IngestResult{}, fmt.Errorf("append inbound delivery: %w", err)
	}
	if err := i.store.SetInboundStreamID(ctx, delivery.DeliveryID, streamID); err != nil {
		return IngestResult{}, fmt.Errorf("record inbound stream position: %w", err)
	}
	return result, nil
}

func (i *Inbound) Run(ctx context.Context) error {
	if err := i.queue.EnsureGroup(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		messages, err := i.queue.ReadNew(ctx, i.config.Consumer, i.config.PollInterval, i.config.BatchSize)
		if err != nil && ctx.Err() == nil {
			return err
		}
		for _, message := range messages {
			_ = i.process(ctx, message)
		}
		claimed, err := i.queue.ClaimStale(ctx, i.config.Consumer, i.config.ClaimMinIdle, i.config.BatchSize)
		if err != nil && ctx.Err() == nil {
			return err
		}
		for _, message := range claimed {
			_ = i.process(ctx, message)
		}
	}
	return ctx.Err()
}

func (i *Inbound) process(ctx context.Context, message StreamMessage) error {
	delivery, err := i.store.GetInboundDelivery(ctx, message.DeliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return i.queue.Ack(ctx, message.ID)
	}
	if err != nil {
		return err
	}
	if delivery.Status == models.InboundSucceeded || delivery.Status == models.InboundDLQ ||
		delivery.Status == models.InboundRejectedUnknownRoute || delivery.Status == models.InboundRejectedRouteDisabled {
		return i.queue.Ack(ctx, message.ID)
	}
	if delivery.NextAttemptAt != "" {
		next, parseErr := time.Parse(time.RFC3339Nano, delivery.NextAttemptAt)
		if parseErr == nil && time.Now().Before(next) {
			return nil
		}
	}

	attemptID := uuid.NewString()
	attempt, err := i.store.BeginInboundAttempt(ctx, delivery.DeliveryID, attemptID)
	if err != nil {
		return err
	}
	status, class := i.deliver(ctx, delivery, attemptID, attempt)
	if status >= 200 && status < 300 && class == "" {
		if err := i.store.CompleteInboundAttempt(ctx, delivery.DeliveryID, attemptID, status); err != nil {
			return err
		}
		return i.queue.Ack(ctx, message.ID)
	}

	next := time.Now().Add(i.retryDelay(delivery.DeliveryID, attempt))
	if err := i.store.FailInboundAttempt(ctx, delivery.DeliveryID, attemptID, class, status, next); err != nil {
		return err
	}
	if attempt < i.config.MaxAttempts {
		return nil
	}
	if err := i.queue.MoveToDLQAndAck(ctx, message, class, attempt); err != nil {
		return err
	}
	return i.store.MarkInboundDLQ(ctx, delivery.DeliveryID, class)
}

func (i *Inbound) deliver(ctx context.Context, delivery *models.InboundDelivery, attemptID string, attempt int) (int, string) {
	if delivery.BackendURL == "" || delivery.BackendToken == "" {
		return 0, "backend_not_configured"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.BackendURL, jsonBytesReader(delivery.RawUpdate))
	if err != nil {
		return 0, "backend_request_invalid"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+delivery.BackendToken)
	req.Header.Set("X-Gateway-Route-Key", delivery.RouteKey)
	req.Header.Set("X-Gateway-Delivery-ID", delivery.DeliveryID)
	req.Header.Set("X-Gateway-Attempt-ID", attemptID)
	req.Header.Set("X-Gateway-Attempt", strconv.Itoa(attempt))
	resp, err := i.client.Do(req)
	if err != nil {
		return 0, "backend_unavailable"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, ""
	}
	return resp.StatusCode, "backend_non_2xx"
}

func (i *Inbound) retryDelay(deliveryID string, attempt int) time.Duration {
	delay := i.config.BaseRetry
	for n := 1; n < attempt && delay < i.config.MaxRetry; n++ {
		delay *= 2
	}
	if delay > i.config.MaxRetry {
		delay = i.config.MaxRetry
	}
	// Stable +/-20% jitter prevents synchronized retries and keeps tests deterministic.
	var hash uint32
	for _, char := range deliveryID {
		hash = hash*33 + uint32(char)
	}
	jitter := (float64(int(hash%41)-20) / 100.0) + 1
	return time.Duration(float64(delay) * jitter)
}
