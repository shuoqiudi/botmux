package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

var (
	ErrUnauthorized        = errors.New("invalid workload credential")
	ErrForbidden           = errors.New("workload is not permitted to perform this action")
	ErrUnknownRoute        = errors.New("business route not found")
	ErrRouteDisabled       = errors.New("business route is not enabled for outbound delivery")
	ErrIdempotencyRequired = errors.New("a valid Idempotency-Key is required")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different payload")
	ErrInvalidPayload      = errors.New("invalid outbound payload")
	ErrQueueUnavailable    = errors.New("durable delivery queue is unavailable")
)

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type Repository interface {
	AuthenticateGatewayWorkload(context.Context, string) (*models.GatewayWorkload, error)
	GatewayRouteForAction(context.Context, int64, string, models.GatewayAction) (*models.BusinessRoute, bool, error)
	CreateGatewayOutboundDelivery(context.Context, models.GatewayDeliveryCreate) (*models.GatewayDelivery, bool, error)
	MarkGatewayDeliveryAccepted(context.Context, string) error
	GetGatewayDelivery(context.Context, int64, string) (*models.GatewayDelivery, error)
	GetGatewayDeliveryForWorker(context.Context, string) (*models.GatewayDelivery, []byte, error)
	ResolveGatewayOutboundTarget(context.Context, int64) (*models.GatewayOutboundTarget, error)
	BeginGatewayDeliveryAttempt(context.Context, string, string, string) (int, error)
	MarkGatewayDeliverySucceeded(context.Context, string, *int64) error
	MarkGatewayDeliveryRetrying(context.Context, string, string, string, time.Time) error
	MarkGatewayDeliveryReconciling(context.Context, string, string, string) error
	MarkGatewayDeliveryDeadLettered(context.Context, string, string, string, string, int) error
	CompleteGatewayDeliveryAttempt(context.Context, string) error
	GatewayBotRateLimit(context.Context, int64) (time.Time, error)
	SetGatewayBotRateLimit(context.Context, int64, time.Time) error
}

type QueueMessage struct {
	ID         string
	DeliveryID string
}

type OutboundQueue interface {
	Append(context.Context, string) (string, error)
	Read(context.Context, string, int64) ([]QueueMessage, error)
	ClaimStale(context.Context, string, time.Duration, int64) ([]QueueMessage, error)
	Ack(context.Context, string) error
	MoveToDLQAndAck(context.Context, QueueMessage, DLQMetadata) error
}

type DLQMetadata struct {
	RouteKey     string
	AttemptCount int
	ErrorClass   string
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type ReplyMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type ReplyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply,omitempty"`
}

type SendMessage struct {
	MessageThreadID int64            `json:"message_thread_id,omitempty"`
	Text            string           `json:"text"`
	ParseMode       string           `json:"parse_mode,omitempty"`
	ReplyMarkup     *ReplyMarkup     `json:"reply_markup,omitempty"`
	ReplyParameters *ReplyParameters `json:"reply_parameters,omitempty"`
}

type AnswerCallback struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

type outboundEnvelope struct {
	Kind     string          `json:"kind"`
	Message  *SendMessage    `json:"message,omitempty"`
	Callback *AnswerCallback `json:"callback,omitempty"`
}

type Telegram interface {
	SendMessage(context.Context, string, int64, SendMessage) (int64, error)
	AnswerCallback(context.Context, string, AnswerCallback) error
}

type Service struct {
	repo     Repository
	queue    OutboundQueue
	telegram Telegram
	consumer string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	config   OutboundConfig
	gateMu   sync.Mutex
	botGates map[int64]time.Time
	health   workerHeartbeat
}

func NewService(repo Repository, queue OutboundQueue, telegram Telegram, consumer string) *Service {
	return NewServiceWithConfig(repo, queue, telegram, OutboundConfig{Consumer: consumer})
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type OutboundConfig struct {
	Consumer     string
	MaxAttempts  int
	BaseRetry    time.Duration
	MaxRetry     time.Duration
	ClaimMinIdle time.Duration
	BatchSize    int64
	Clock        Clock
	Backoff      func(string, int) time.Duration
}

func NewServiceWithConfig(repo Repository, queue OutboundQueue, telegram Telegram, config OutboundConfig) *Service {
	consumer := config.Consumer
	if consumer == "" {
		consumer = newOpaqueID()
	}
	config.Consumer = consumer
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 5
	}
	if config.BaseRetry <= 0 {
		config.BaseRetry = time.Second
	}
	if config.MaxRetry <= 0 {
		config.MaxRetry = 30 * time.Second
	}
	if config.ClaimMinIdle <= 0 {
		config.ClaimMinIdle = time.Second
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 16
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	return &Service{repo: repo, queue: queue, telegram: telegram, consumer: consumer, config: config,
		botGates: make(map[int64]time.Time)}
}

func (s *Service) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.health.start()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.health.stop()
		s.runOutbound(ctx)
	}()
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func ValidateIdempotencyKey(key string) error {
	if !idempotencyKeyRE.MatchString(key) {
		return ErrIdempotencyRequired
	}
	return nil
}

func ValidateSendMessage(message SendMessage) error {
	if strings.TrimSpace(message.Text) == "" || len(message.Text) > 4096 {
		return fmt.Errorf("%w: text must contain 1 to 4096 bytes", ErrInvalidPayload)
	}
	switch message.ParseMode {
	case "", "HTML", "Markdown", "MarkdownV2":
	default:
		return fmt.Errorf("%w: parse_mode is not allowed", ErrInvalidPayload)
	}
	if message.ReplyParameters != nil && message.ReplyParameters.MessageID <= 0 {
		return fmt.Errorf("%w: reply_parameters.message_id must be positive", ErrInvalidPayload)
	}
	if message.ReplyMarkup == nil {
		return nil
	}
	if len(message.ReplyMarkup.InlineKeyboard) == 0 || len(message.ReplyMarkup.InlineKeyboard) > 100 {
		return fmt.Errorf("%w: inline_keyboard must contain 1 to 100 rows", ErrInvalidPayload)
	}
	buttons := 0
	for _, row := range message.ReplyMarkup.InlineKeyboard {
		if len(row) == 0 || len(row) > 8 {
			return fmt.Errorf("%w: inline keyboard rows must contain 1 to 8 buttons", ErrInvalidPayload)
		}
		for _, button := range row {
			buttons++
			if strings.TrimSpace(button.Text) == "" || len(button.Text) > 64 || len(button.CallbackData) == 0 || len(button.CallbackData) > 64 {
				return fmt.Errorf("%w: inline keyboard button text and callback_data are invalid", ErrInvalidPayload)
			}
		}
	}
	if buttons > 100 {
		return fmt.Errorf("%w: inline keyboard contains too many buttons", ErrInvalidPayload)
	}
	return nil
}

func ValidateAnswerCallback(callback AnswerCallback) error {
	if callback.CallbackQueryID == "" || len(callback.CallbackQueryID) > 256 || len(callback.Text) > 200 {
		return ErrInvalidPayload
	}
	return nil
}

func (s *Service) Authenticate(ctx context.Context, credential string) (*models.GatewayWorkload, error) {
	if credential == "" {
		return nil, ErrUnauthorized
	}
	workload, err := s.repo.AuthenticateGatewayWorkload(ctx, auth.HashAPIKey(credential))
	if err != nil {
		return nil, ErrUnauthorized
	}
	return workload, nil
}

func (s *Service) AcceptMessage(ctx context.Context, workload *models.GatewayWorkload, routeKey, idempotencyKey string, message SendMessage) (*models.GatewayDelivery, error) {
	if err := ValidateSendMessage(message); err != nil {
		return nil, err
	}
	return s.accept(ctx, workload, routeKey, idempotencyKey, models.GatewayActionMessagesSend,
		outboundEnvelope{Kind: "message", Message: &message})
}

func (s *Service) AcceptCallback(ctx context.Context, workload *models.GatewayWorkload, routeKey, idempotencyKey string, callback AnswerCallback) (*models.GatewayDelivery, error) {
	if err := ValidateAnswerCallback(callback); err != nil {
		return nil, err
	}
	return s.accept(ctx, workload, routeKey, idempotencyKey, models.GatewayActionCallbacksAnswer,
		outboundEnvelope{Kind: "callback", Callback: &callback})
}

func (s *Service) accept(ctx context.Context, workload *models.GatewayWorkload, routeKey, key string, action models.GatewayAction, envelope outboundEnvelope) (*models.GatewayDelivery, error) {
	if workload == nil {
		return nil, ErrUnauthorized
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return nil, err
	}
	route, allowed, err := s.repo.GatewayRouteForAction(ctx, workload.ID, routeKey, action)
	if err != nil {
		return nil, ErrUnknownRoute
	}
	if !allowed {
		return nil, ErrForbidden
	}
	if !route.Enabled || route.Status != models.GatewayRouteActive || !route.OutboundEnabled {
		return nil, ErrRouteDisabled
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(payload)
	delivery, _, err := s.repo.CreateGatewayOutboundDelivery(ctx, models.GatewayDeliveryCreate{
		ID: newOpaqueID(), RouteID: route.ID, RouteRevision: route.Revision, WorkloadID: workload.ID,
		Action: action, PayloadHash: hex.EncodeToString(hash[:]), IdempotencyKey: key, PayloadJSON: payload,
	})
	if errors.Is(err, store.ErrGatewayIdempotencyConflict) {
		return nil, ErrIdempotencyConflict
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.queue.Append(ctx, delivery.ID); err != nil {
		return nil, ErrQueueUnavailable
	}
	if err := s.repo.MarkGatewayDeliveryAccepted(ctx, delivery.ID); err != nil {
		return nil, err
	}
	delivery.Status = models.GatewayDeliveryAccepted
	return delivery, nil
}

func (s *Service) GetDelivery(ctx context.Context, workload *models.GatewayWorkload, routeKey, deliveryID string) (*models.GatewayDelivery, error) {
	if workload == nil {
		return nil, ErrUnauthorized
	}
	route, allowed, err := s.repo.GatewayRouteForAction(ctx, workload.ID, routeKey, models.GatewayActionDeliveriesRead)
	if err != nil {
		return nil, ErrUnknownRoute
	}
	if !allowed {
		return nil, ErrForbidden
	}
	delivery, err := s.repo.GetGatewayDelivery(ctx, route.ID, deliveryID)
	if err != nil {
		return nil, errors.New("delivery_not_found")
	}
	return delivery, nil
}

func newOpaqueID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("crypto/rand unavailable")
	}
	return hex.EncodeToString(raw[:])
}

func (s *Service) runOutbound(ctx context.Context) {
	for ctx.Err() == nil {
		s.health.beat()
		messages, err := s.queue.Read(ctx, s.consumer, s.config.BatchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		for _, message := range messages {
			s.health.beat()
			s.processOutbound(ctx, message)
		}
		claimed, err := s.queue.ClaimStale(ctx, s.consumer, s.config.ClaimMinIdle, s.config.BatchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		for _, message := range claimed {
			s.health.beat()
			s.processOutbound(ctx, message)
		}
	}
}

func (s *Service) WorkerHealth() (bool, time.Time) { return s.health.snapshot() }

func (s *Service) processOutbound(ctx context.Context, queued QueueMessage) {
	var delivery *models.GatewayDelivery
	var payload []byte
	var err error
	loadDeadline := time.Now().Add(time.Second)
	for {
		delivery, payload, err = s.repo.GetGatewayDeliveryForWorker(ctx, queued.DeliveryID)
		if err == nil || time.Now().After(loadDeadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err != nil {
		return
	}
	// XADD must precede the accepted state transition. A local worker can see
	// the Stream entry during that tiny dual-write window, so wait for the
	// accepting request to finish instead of consuming the item prematurely.
	acceptanceDeadline := time.Now().Add(time.Second)
	for delivery.Status == models.GatewayDeliveryPendingEnqueue && time.Now().Before(acceptanceDeadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
		delivery, payload, err = s.repo.GetGatewayDeliveryForWorker(ctx, queued.DeliveryID)
		if err != nil {
			return
		}
	}
	if delivery.Status == models.GatewayDeliveryPendingEnqueue {
		return
	}
	if delivery.Status == models.GatewayDeliverySucceeded || delivery.Status == models.GatewayDeliveryReconciling {
		_ = s.queue.Ack(ctx, queued.ID)
		return
	}
	if delivery.Status == models.GatewayDeliveryDeadLettered {
		_ = s.queue.MoveToDLQAndAck(ctx, queued, DLQMetadata{RouteKey: delivery.RouteKey,
			AttemptCount: delivery.AttemptCount, ErrorClass: delivery.SafeErrorClass})
		return
	}
	// A reclaimed processing item may have reached Telegram before its worker
	// disappeared. Telegram has no outbound idempotency primitive, so never
	// blindly send it again.
	if delivery.Status == models.GatewayDeliveryProcessing {
		if err := s.repo.MarkGatewayDeliveryReconciling(ctx, delivery.ID, "", "worker_interrupted_after_dispatch"); err == nil {
			_ = s.queue.Ack(ctx, queued.ID)
		}
		return
	}
	if delivery.NextAttemptAt != "" {
		next, parseErr := time.Parse(time.RFC3339Nano, delivery.NextAttemptAt)
		if parseErr == nil && s.config.Clock.Now().Before(next) {
			return
		}
	}
	target, err := s.repo.ResolveGatewayOutboundTarget(ctx, delivery.RouteID)
	if resolver, ok := s.repo.(interface {
		ResolveAdapterReplyTarget(context.Context, string) (*models.GatewayOutboundTarget, error)
	}); ok {
		pinned, pinErr := resolver.ResolveAdapterReplyTarget(ctx, delivery.ID)
		if pinErr != nil {
			return
		}
		if pinned != nil {
			target, err = pinned, nil
		}
	}
	if err != nil || !target.Enabled || !target.Outbound {
		s.deadLetter(ctx, queued, delivery, "", "route_disabled", delivery.AttemptCount)
		return
	}
	gate, err := s.botGate(ctx, target.BotAccountID)
	if err != nil {
		return
	}
	if s.config.Clock.Now().Before(gate) {
		_ = s.repo.MarkGatewayDeliveryRetrying(ctx, delivery.ID, "", "telegram_rate_limited", gate)
		return
	}
	attemptID := newOpaqueID()
	attempt, err := s.repo.BeginGatewayDeliveryAttempt(ctx, delivery.ID, attemptID, s.consumer)
	if err != nil {
		return
	}
	var envelope outboundEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		s.deadLetter(ctx, queued, delivery, attemptID, "invalid_stored_payload", attempt)
		return
	}
	var messageID *int64
	switch envelope.Kind {
	case "message":
		if envelope.Message == nil {
			err = ErrInvalidPayload
			break
		}
		var sent int64
		sent, err = s.telegram.SendMessage(ctx, target.Token, target.ChatID, *envelope.Message)
		messageID = &sent
	case "callback":
		if envelope.Callback == nil {
			err = ErrInvalidPayload
			break
		}
		err = s.telegram.AnswerCallback(ctx, target.Token, *envelope.Callback)
	default:
		err = ErrInvalidPayload
	}
	if err == nil {
		if completeErr := s.repo.CompleteGatewayDeliveryAttempt(ctx, attemptID); completeErr == nil {
			if err := s.repo.MarkGatewayDeliverySucceeded(ctx, delivery.ID, messageID); err == nil {
				_ = s.queue.Ack(ctx, queued.ID)
			}
		} else {
			// Leave processing in the PEL. A future worker will conservatively
			// reconcile the send instead of duplicating it.
			return
		}
		return
	}
	var telegramErr *TelegramError
	if errors.As(err, &telegramErr) {
		if telegramErr.Outcome == TelegramAmbiguous {
			if err := s.repo.MarkGatewayDeliveryReconciling(ctx, delivery.ID, attemptID, telegramErr.Class); err == nil {
				_ = s.queue.Ack(ctx, queued.ID)
			}
			return
		}
		if telegramErr.Retryable() && attempt < s.config.MaxAttempts {
			delay := s.retryDelay(delivery.ID, attempt)
			if telegramErr.RetryAfter > delay {
				delay = telegramErr.RetryAfter
			}
			next := s.config.Clock.Now().Add(delay)
			if telegramErr.Class == "telegram_rate_limited" {
				if err := s.setBotGate(ctx, target.BotAccountID, next); err != nil {
					return
				}
			}
			_ = s.repo.MarkGatewayDeliveryRetrying(ctx, delivery.ID, attemptID, telegramErr.Class, next)
			return
		}
		if telegramErr.Retryable() {
			s.deadLetter(ctx, queued, delivery, attemptID, telegramErr.Class, attempt)
			return
		}
	}
	class := "telegram_rejected"
	if errors.Is(err, ErrInvalidPayload) {
		class = "invalid_stored_payload"
	}
	if telegramErr != nil && telegramErr.Class != "" {
		class = telegramErr.Class
	}
	s.deadLetter(ctx, queued, delivery, attemptID, class, attempt)
}

func (s *Service) retryDelay(deliveryID string, attempt int) time.Duration {
	if s.config.Backoff != nil {
		return s.config.Backoff(deliveryID, attempt)
	}
	return retryBackoff(deliveryID, attempt, s.config.BaseRetry, s.config.MaxRetry)
}

func (s *Service) botGate(ctx context.Context, botID int64) (time.Time, error) {
	s.gateMu.Lock()
	memoryGate := s.botGates[botID]
	s.gateMu.Unlock()
	durableGate, err := s.repo.GatewayBotRateLimit(ctx, botID)
	if err != nil {
		return time.Time{}, err
	}
	if durableGate.After(memoryGate) {
		return durableGate, nil
	}
	return memoryGate, nil
}

func (s *Service) setBotGate(ctx context.Context, botID int64, deadline time.Time) error {
	if err := s.repo.SetGatewayBotRateLimit(ctx, botID, deadline); err != nil {
		return err
	}
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	if deadline.After(s.botGates[botID]) {
		s.botGates[botID] = deadline
	}
	return nil
}

func (s *Service) deadLetter(ctx context.Context, queued QueueMessage, delivery *models.GatewayDelivery, attemptID, class string, attempts int) {
	// SQLite records the durable terminal item first. The Redis script then
	// atomically writes the Stream DLQ entry before acknowledging the source.
	// If the process stops between them, reclaim sees dead-lettered and retries
	// the idempotent Stream move.
	if err := s.repo.MarkGatewayDeliveryDeadLettered(ctx, delivery.ID, attemptID, queued.ID, class, attempts); err != nil {
		return
	}
	_ = s.queue.MoveToDLQAndAck(ctx, queued, DLQMetadata{RouteKey: delivery.RouteKey, AttemptCount: attempts, ErrorClass: class})
}
