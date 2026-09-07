package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

type faultClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *faultClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *faultClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type faultRepository struct {
	mu         sync.Mutex
	deliveries map[string]*models.GatewayDelivery
	payloads   map[string][]byte
	targets    map[int64]*models.GatewayOutboundTarget
	botGates   map[int64]time.Time
}

func newFaultRepository(ids ...string) *faultRepository {
	r := &faultRepository{deliveries: make(map[string]*models.GatewayDelivery), payloads: make(map[string][]byte), botGates: make(map[int64]time.Time),
		targets: map[int64]*models.GatewayOutboundTarget{1: {
			RouteKey: "alerts", BotAccountID: 42, Enabled: true, Outbound: true, Token: "test-token", ChatID: -1001,
		}}}
	for _, id := range ids {
		r.deliveries[id] = &models.GatewayDelivery{ID: id, RouteID: 1, RouteKey: "alerts", Status: "accepted"}
		r.payloads[id] = []byte(`{"kind":"message","message":{"text":"safe test"}}`)
	}
	return r
}

func (r *faultRepository) AuthenticateGatewayWorkload(context.Context, string) (*models.GatewayWorkload, error) {
	return nil, errors.New("unused")
}
func (r *faultRepository) GatewayRouteForAction(context.Context, int64, string, models.GatewayAction) (*models.BusinessRoute, bool, error) {
	return nil, false, errors.New("unused")
}
func (r *faultRepository) CreateGatewayOutboundDelivery(context.Context, models.GatewayDeliveryCreate) (*models.GatewayDelivery, bool, error) {
	return nil, false, errors.New("unused")
}
func (r *faultRepository) MarkGatewayDeliveryAccepted(context.Context, string) error { return nil }
func (r *faultRepository) GetGatewayDelivery(context.Context, int64, string) (*models.GatewayDelivery, error) {
	return nil, errors.New("unused")
}
func (r *faultRepository) GetGatewayDeliveryForWorker(_ context.Context, id string) (*models.GatewayDelivery, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := *r.deliveries[id]
	return &d, append([]byte(nil), r.payloads[id]...), nil
}
func (r *faultRepository) ResolveGatewayOutboundTarget(_ context.Context, routeID int64) (*models.GatewayOutboundTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := *r.targets[routeID]
	return &t, nil
}
func (r *faultRepository) BeginGatewayDeliveryAttempt(_ context.Context, id, _, _ string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deliveries[id]
	if d.Status != "accepted" && d.Status != "retrying" {
		return 0, errors.New("not dispatchable")
	}
	d.Status = "processing"
	d.AttemptCount++
	d.NextAttemptAt = ""
	return d.AttemptCount, nil
}
func (r *faultRepository) MarkGatewayDeliverySucceeded(_ context.Context, id string, _ *int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries[id].Status = "succeeded"
	r.deliveries[id].SafeErrorClass = ""
	return nil
}
func (r *faultRepository) MarkGatewayDeliveryRetrying(_ context.Context, id, _, class string, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deliveries[id]
	d.Status = "retrying"
	d.SafeErrorClass = class
	d.NextAttemptAt = next.Format(time.RFC3339Nano)
	return nil
}
func (r *faultRepository) MarkGatewayDeliveryReconciling(_ context.Context, id, _, class string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deliveries[id]
	d.Status = "reconciling"
	d.SafeErrorClass = class
	return nil
}
func (r *faultRepository) MarkGatewayDeliveryDeadLettered(_ context.Context, id, _, _, class string, attempts int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deliveries[id]
	d.Status = "dead-lettered"
	d.SafeErrorClass = class
	d.AttemptCount = attempts
	return nil
}
func (r *faultRepository) CompleteGatewayDeliveryAttempt(context.Context, string) error { return nil }
func (r *faultRepository) GatewayBotRateLimit(_ context.Context, botID int64) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.botGates[botID], nil
}
func (r *faultRepository) SetGatewayBotRateLimit(_ context.Context, botID int64, deadline time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if deadline.After(r.botGates[botID]) {
		r.botGates[botID] = deadline
	}
	return nil
}

func (r *faultRepository) delivery(id string) models.GatewayDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.deliveries[id]
}

type faultQueue struct {
	mu      sync.Mutex
	acked   []string
	dlq     []DLQMetadata
	claimed []QueueMessage
}

func (q *faultQueue) Append(context.Context, string) (string, error) { return "1-0", nil }
func (q *faultQueue) Read(context.Context, string, int64) ([]QueueMessage, error) {
	return nil, nil
}
func (q *faultQueue) ClaimStale(context.Context, string, time.Duration, int64) ([]QueueMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := append([]QueueMessage(nil), q.claimed...)
	q.claimed = nil
	return result, nil
}
func (q *faultQueue) Ack(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.acked = append(q.acked, id)
	return nil
}
func (q *faultQueue) MoveToDLQAndAck(_ context.Context, message QueueMessage, metadata DLQMetadata) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dlq = append(q.dlq, metadata)
	q.acked = append(q.acked, message.ID)
	return nil
}

type telegramResult struct {
	id  int64
	err error
}

type faultTelegram struct {
	mu      sync.Mutex
	results []telegramResult
	calls   int
}

func (t *faultTelegram) SendMessage(context.Context, string, int64, SendMessage) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	result := t.results[0]
	t.results = t.results[1:]
	return result.id, result.err
}
func (t *faultTelegram) AnswerCallback(context.Context, string, AnswerCallback) error {
	return errors.New("unused")
}

func newFaultService(repo *faultRepository, queue *faultQueue, telegram *faultTelegram, clock *faultClock, max int, backoff func(string, int) time.Duration) *Service {
	return NewServiceWithConfig(repo, queue, telegram, OutboundConfig{Consumer: "fault-worker", MaxAttempts: max,
		ClaimMinIdle: time.Nanosecond, Clock: clock, Backoff: backoff})
}

func TestOutboundFaultLifecycleSerial(t *testing.T) {
	t.Run("bounded retry and exhaustion", func(t *testing.T) {
		clock := &faultClock{now: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)}
		repo := newFaultRepository("delivery-1")
		queue := &faultQueue{}
		telegram := &faultTelegram{results: []telegramResult{
			{err: &TelegramError{Class: "telegram_unavailable", Outcome: TelegramDefiniteTransient}},
			{err: &TelegramError{Class: "telegram_unavailable", Outcome: TelegramDefiniteTransient}},
		}}
		var attempts []int
		service := newFaultService(repo, queue, telegram, clock, 2, func(_ string, attempt int) time.Duration {
			attempts = append(attempts, attempt)
			return time.Duration(attempt) * time.Minute
		})
		message := QueueMessage{ID: "stream-1", DeliveryID: "delivery-1"}
		service.processOutbound(context.Background(), message)
		if got := repo.delivery("delivery-1"); got.Status != "retrying" || got.AttemptCount != 1 {
			t.Fatalf("first failure = %+v", got)
		}
		service.processOutbound(context.Background(), message)
		if telegram.calls != 1 {
			t.Fatalf("retry ran before controlled deadline: %d calls", telegram.calls)
		}
		clock.Advance(time.Minute)
		service.processOutbound(context.Background(), message)
		got := repo.delivery("delivery-1")
		if got.Status != "dead-lettered" || got.AttemptCount != 2 || len(queue.dlq) != 1 || len(queue.acked) != 1 {
			t.Fatalf("exhausted delivery=%+v dlq=%+v ack=%v", got, queue.dlq, queue.acked)
		}
		if len(attempts) != 1 || attempts[0] != 1 || queue.dlq[0].RouteKey != "alerts" || queue.dlq[0].ErrorClass != "telegram_unavailable" {
			t.Fatalf("unsafe or incomplete retry/DLQ metadata: attempts=%v dlq=%+v", attempts, queue.dlq)
		}
	})

	t.Run("429 gates all deliveries for one bot", func(t *testing.T) {
		clock := &faultClock{now: time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)}
		repo := newFaultRepository("rate-limited", "same-bot")
		queue := &faultQueue{}
		telegram := &faultTelegram{results: []telegramResult{
			{err: &TelegramError{Class: "telegram_rate_limited", Outcome: TelegramDefiniteTransient, RetryAfter: 7 * time.Second}},
			{id: 91}, {id: 92},
		}}
		service := newFaultService(repo, queue, telegram, clock, 3, func(string, int) time.Duration { return time.Second })
		service.processOutbound(context.Background(), QueueMessage{ID: "s1", DeliveryID: "rate-limited"})
		// A replacement worker has an empty in-memory gate and must still obey
		// the durable bot-account deadline.
		restarted := newFaultService(repo, queue, telegram, clock, 3, func(string, int) time.Duration { return time.Second })
		restarted.processOutbound(context.Background(), QueueMessage{ID: "s2", DeliveryID: "same-bot"})
		if telegram.calls != 1 || repo.delivery("same-bot").AttemptCount != 0 || repo.delivery("same-bot").Status != "retrying" {
			t.Fatalf("per-bot gate was not applied: calls=%d second=%+v", telegram.calls, repo.delivery("same-bot"))
		}
		clock.Advance(7 * time.Second)
		restarted.processOutbound(context.Background(), QueueMessage{ID: "s2", DeliveryID: "same-bot"})
		service.processOutbound(context.Background(), QueueMessage{ID: "s1", DeliveryID: "rate-limited"})
		if telegram.calls != 3 || repo.delivery("same-bot").Status != "succeeded" || repo.delivery("rate-limited").Status != "succeeded" {
			t.Fatalf("deliveries did not resume after retry_after: calls=%d first=%+v second=%+v", telegram.calls,
				repo.delivery("rate-limited"), repo.delivery("same-bot"))
		}
	})

	t.Run("ambiguous result and interrupted dispatch reconcile without resend", func(t *testing.T) {
		clock := &faultClock{now: time.Now()}
		repo := newFaultRepository("ambiguous", "interrupted")
		repo.deliveries["interrupted"].Status = "processing"
		repo.deliveries["interrupted"].AttemptCount = 1
		queue := &faultQueue{}
		telegram := &faultTelegram{results: []telegramResult{{err: &TelegramError{
			Class: "telegram_send_ambiguous", Outcome: TelegramAmbiguous, WriteObserved: true,
		}}}}
		service := newFaultService(repo, queue, telegram, clock, 3, nil)
		ambiguous := QueueMessage{ID: "s1", DeliveryID: "ambiguous"}
		service.processOutbound(context.Background(), ambiguous)
		service.processOutbound(context.Background(), ambiguous)
		service.processOutbound(context.Background(), QueueMessage{ID: "s2", DeliveryID: "interrupted"})
		if telegram.calls != 1 || repo.delivery("ambiguous").Status != "reconciling" || repo.delivery("interrupted").Status != "reconciling" {
			t.Fatalf("ambiguous sends were retried: calls=%d ambiguous=%+v interrupted=%+v", telegram.calls,
				repo.delivery("ambiguous"), repo.delivery("interrupted"))
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramHTTPFailureClassificationAndRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		transport  roundTripFunc
		outcome    TelegramFailureOutcome
		retryAfter time.Duration
	}{
		{name: "pre-write is definitely retryable", outcome: TelegramDefiniteTransient, transport: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connect failed")
		}},
		{name: "post-write is ambiguous", outcome: TelegramAmbiguous, transport: func(req *http.Request) (*http.Response, error) {
			httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
			return nil, errors.New("response lost")
		}},
		{name: "429 reads nested retry_after", outcome: TelegramDefiniteTransient, retryAfter: 11 * time.Second, transport: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(
				`{"ok":false,"error_code":429,"parameters":{"retry_after":11}}`)), Header: make(http.Header)}, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := NewTelegramHTTPClientWithClient("https://telegram.invalid", &http.Client{Transport: test.transport})
			_, err := client.SendMessage(context.Background(), "token", 1, SendMessage{Text: "safe"})
			var telegramErr *TelegramError
			if !errors.As(err, &telegramErr) || telegramErr.Outcome != test.outcome || telegramErr.RetryAfter != test.retryAfter {
				t.Fatalf("classification = %#v, error=%v", telegramErr, err)
			}
		})
	}
}
