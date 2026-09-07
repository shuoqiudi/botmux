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
	GatewayRouteForAction(context.Context, int64, string, string) (*models.BusinessRoute, bool, error)
	CreateGatewayOutboundDelivery(context.Context, models.GatewayDeliveryCreate) (*models.GatewayDelivery, bool, error)
	MarkGatewayDeliveryAccepted(context.Context, string) error
	GetGatewayDelivery(context.Context, int64, string) (*models.GatewayDelivery, error)
	GetGatewayDeliveryForWorker(context.Context, string) (*models.GatewayDelivery, []byte, error)
	ResolveGatewayOutboundTarget(context.Context, int64) (*models.GatewayOutboundTarget, error)
	MarkGatewayDeliveryProcessing(context.Context, string) error
	MarkGatewayDeliverySucceeded(context.Context, string, *int64) error
	MarkGatewayDeliveryFailed(context.Context, string, string, string) error
}

type QueueMessage struct {
	ID         string
	DeliveryID string
}

type OutboundQueue interface {
	Append(context.Context, string) (string, error)
	Read(context.Context, string, int64) ([]QueueMessage, error)
	Ack(context.Context, string) error
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
}

func NewService(repo Repository, queue OutboundQueue, telegram Telegram, consumer string) *Service {
	if consumer == "" {
		consumer = newOpaqueID()
	}
	return &Service{repo: repo, queue: queue, telegram: telegram, consumer: consumer}
}

func (s *Service) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
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

func (s *Service) accept(ctx context.Context, workload *models.GatewayWorkload, routeKey, key, action string, envelope outboundEnvelope) (*models.GatewayDelivery, error) {
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
	if !route.Enabled || route.Status != "active" || !route.OutboundEnabled {
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
	delivery.Status = "accepted"
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
		messages, err := s.queue.Read(ctx, s.consumer, 16)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		for _, message := range messages {
			s.processOutbound(ctx, message)
		}
	}
}

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
	for delivery.Status == "pending_enqueue" && time.Now().Before(acceptanceDeadline) {
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
	if delivery.Status == "pending_enqueue" {
		return
	}
	if delivery.Status == "succeeded" || delivery.Status == "failed" {
		_ = s.queue.Ack(ctx, queued.ID)
		return
	}
	target, err := s.repo.ResolveGatewayOutboundTarget(ctx, delivery.RouteID)
	if err != nil || !target.Enabled || !target.Outbound {
		_ = s.repo.MarkGatewayDeliveryFailed(ctx, delivery.ID, "failed", "route_disabled")
		_ = s.queue.Ack(ctx, queued.ID)
		return
	}
	if err := s.repo.MarkGatewayDeliveryProcessing(ctx, delivery.ID); err != nil {
		return
	}
	var envelope outboundEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		_ = s.repo.MarkGatewayDeliveryFailed(ctx, delivery.ID, "failed", "invalid_stored_payload")
		_ = s.queue.Ack(ctx, queued.ID)
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
		if err := s.repo.MarkGatewayDeliverySucceeded(ctx, delivery.ID, messageID); err == nil {
			_ = s.queue.Ack(ctx, queued.ID)
		}
		return
	}
	var telegramErr *TelegramError
	if errors.As(err, &telegramErr) && telegramErr.Retryable {
		_ = s.repo.MarkGatewayDeliveryFailed(ctx, delivery.ID, "retrying", telegramErr.Class)
		return
	}
	class := "telegram_rejected"
	if errors.Is(err, ErrInvalidPayload) {
		class = "invalid_stored_payload"
	}
	_ = s.repo.MarkGatewayDeliveryFailed(ctx, delivery.ID, "failed", class)
	_ = s.queue.Ack(ctx, queued.ID)
}
