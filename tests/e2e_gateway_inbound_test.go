package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

type backendReceipt struct {
	Update  map[string]any
	Headers http.Header
}

// TestE2E_GatewayInbound is intentionally serial. It uses the existing fake
// Telegram poller, a real Redis >=6.2 Stream, SQLite, and fake HTTP Backends.
func TestE2E_GatewayInbound(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	err := redisClient.Ping(pingCtx).Err()
	pingCancel()
	if err != nil {
		_ = redisClient.Close()
		t.Skip("real Redis integration requires Redis at 127.0.0.1:6379")
	}
	defer redisClient.Close()
	namespace := "botmux:test:" + uuid.NewString()
	queue := gateway.NewRedisInboundQueueWithNamespace(redisClient, namespace)
	if err := queue.VerifyDurability(context.Background()); err != nil {
		t.Fatalf("Redis integration must use durable AOF: %v", err)
	}

	const backendCredential = "backend-only-shared-secret"
	var receiptsMu sync.Mutex
	var receipts []backendReceipt
	var update100Attempts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var update map[string]any
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode backend Update: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		receiptsMu.Lock()
		receipts = append(receipts, backendReceipt{Update: update, Headers: r.Header.Clone()})
		receiptsMu.Unlock()
		updateID, _ := update["update_id"].(float64)
		if int64(updateID) == 100 && update100Attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if int64(updateID) == 105 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()
	var wrongBackendCalls atomic.Int32
	wrongBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		wrongBackendCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wrongBackend.Close()

	h := setupE2E(t)
	const token = "810001:inbound-owner-token"
	const otherToken = "810002:other-bot-token"
	const chatID int64 = -100810001
	botID := h.AddBot(models.BotConfig{Name: "inbound owner", Token: token, ProxyEnabled: true, PollingTimeout: 1})
	accountID, err := h.store.AddBotAccount(models.BotAccount{Name: "inbound owner", Username: "testbot", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	destinationID, err := h.store.AddTelegramDestination(models.TelegramDestination{
		Name: "management chat", BotAccountID: accountID, ChatID: chatID, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.store.AddBusinessRoute(models.BusinessRoute{
		RouteKey: "it_manage", DisplayName: "IT Manage", BotAccountID: accountID, DestinationID: destinationID,
		InboundEnabled: true, InboundBackendURL: backend.URL, InboundBackendToken: backendCredential,
		Enabled: true, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A different Bot Account owns the same physical chat ID. Content that names
	// its route must still never select it.
	otherAccountID, err := h.store.AddBotAccount(models.BotAccount{Name: "other bot", Username: "other", Token: otherToken})
	if err != nil {
		t.Fatal(err)
	}
	otherDestinationID, err := h.store.AddTelegramDestination(models.TelegramDestination{
		Name: "same chat other bot", BotAccountID: otherAccountID, ChatID: chatID, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.store.AddBusinessRoute(models.BusinessRoute{
		RouteKey: "wrong_route", DisplayName: "Wrong Bot", BotAccountID: otherAccountID, DestinationID: otherDestinationID,
		InboundEnabled: true, InboundBackendURL: wrongBackend.URL, InboundBackendToken: "wrong-secret",
		Enabled: true, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Configure an exact chat that is deliberately inbound-disabled.
	disabledDestinationID, err := h.store.AddTelegramDestination(models.TelegramDestination{
		Name: "disabled chat", BotAccountID: accountID, ChatID: chatID - 1, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.store.AddBusinessRoute(models.BusinessRoute{
		RouteKey: "disabled_inbound", DisplayName: "Disabled inbound", BotAccountID: accountID,
		DestinationID: disabledDestinationID, InboundEnabled: false, Enabled: true, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}

	inbound := gateway.NewInbound(h.store, queue, nil, gateway.InboundConfig{
		Consumer: "replacement-worker", MaxAttempts: 2, BaseRetry: 5 * time.Millisecond,
		MaxRetry: 10 * time.Millisecond, PollInterval: 5 * time.Millisecond,
		ClaimMinIdle: 5 * time.Millisecond, BatchSize: 10,
	})
	h.proxy.SetInboundGateway(inbound)

	messageUpdate := map[string]any{
		"update_id": float64(100),
		"message": map[string]any{
			"message_id": float64(1), "chat": map[string]any{"id": float64(chatID)},
			"text": "/wrong_route this content must not select a route",
		},
	}
	// Simulate a crash after durable append but before ProxyManager advances the
	// Telegram offset. A crashed consumer then owns the pending Stream entry.
	first, err := inbound.IngestUpdate(context.Background(), botID, messageUpdate)
	if err != nil {
		t.Fatal(err)
	}
	botConfig, _ := h.store.GetBotConfig(botID)
	if botConfig.Offset != 0 {
		t.Fatalf("ingestion unexpectedly advanced Telegram offset to %d", botConfig.Offset)
	}
	if err := queue.EnsureGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.ReadNew(context.Background(), "crashed-worker", 5*time.Millisecond, 1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("crashed worker did not acquire delivery: pending=%v err=%v", pending, err)
	}
	if !h.proxy.ProcessUpdate(botID, messageUpdate) {
		t.Fatal("Telegram replay was not acknowledged")
	}
	replayed, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 100)
	if err != nil || replayed.DeliveryID != first.DeliveryID {
		t.Fatalf("Telegram replay created a second delivery: first=%s replay=%+v err=%v", first.DeliveryID, replayed, err)
	}
	botConfig, _ = h.store.GetBotConfig(botID)
	if botConfig.Offset != 101 {
		t.Fatalf("offset=%d, want 101 after durable replay", botConfig.Offset)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- inbound.Run(workerCtx) }()
	defer func() {
		cancelWorker()
		select {
		case <-workerDone:
		case <-time.After(time.Second):
			t.Error("inbound worker did not stop")
		}
	}()
	h.Eventually(func() bool {
		d, err := h.store.GetInboundDelivery(context.Background(), first.DeliveryID)
		return err == nil && d.Status == models.InboundSucceeded && d.AttemptCount == 2
	}, 2*time.Second, "pending delivery reclaimed and accepted after Backend retry")

	callbackUpdate := map[string]any{
		"update_id": float64(101),
		"callback_query": map[string]any{
			"id": "callback-dedupe-id", "data": "wrong_route:approve",
			"message": map[string]any{"message_id": float64(2), "chat": map[string]any{"id": float64(chatID)}},
		},
	}
	if !h.proxy.ProcessUpdate(botID, callbackUpdate) {
		t.Fatal("callback Update was not durably accepted")
	}
	callbackDelivery, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 101)
	if err != nil {
		t.Fatal(err)
	}
	h.Eventually(func() bool {
		d, err := h.store.GetInboundDelivery(context.Background(), callbackDelivery.DeliveryID)
		return err == nil && d.Status == models.InboundSucceeded
	}, time.Second, "callback delivered using callback message chat")

	duplicateCallback := map[string]any{
		"update_id": float64(102),
		"callback_query": map[string]any{
			"id": "callback-dedupe-id", "data": "different-content",
			"message": map[string]any{"chat": map[string]any{"id": float64(chatID)}},
		},
	}
	if !h.proxy.ProcessUpdate(botID, duplicateCallback) {
		t.Fatal("duplicate callback was not acknowledged")
	}
	if _, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 102); err == nil {
		t.Fatal("duplicate callback created another Delivery")
	}

	unknown := map[string]any{"update_id": float64(103), "message": map[string]any{
		"chat": map[string]any{"id": float64(chatID - 99)}, "text": "/it_manage",
	}}
	if !h.proxy.ProcessUpdate(botID, unknown) {
		t.Fatal("unknown chat should be explicitly rejected and acknowledged")
	}
	unknownDelivery, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 103)
	if err != nil || unknownDelivery.Status != models.InboundRejectedUnknownRoute {
		t.Fatalf("unknown chat outcome=%+v err=%v", unknownDelivery, err)
	}

	disabled := map[string]any{"update_id": float64(104), "message": map[string]any{
		"chat": map[string]any{"id": float64(chatID - 1)}, "text": "anything",
	}}
	if !h.proxy.ProcessUpdate(botID, disabled) {
		t.Fatal("disabled inbound Route should be rejected and acknowledged")
	}
	disabledDelivery, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 104)
	if err != nil || disabledDelivery.Status != models.InboundRejectedRouteDisabled {
		t.Fatalf("disabled route outcome=%+v err=%v", disabledDelivery, err)
	}

	// Every non-2xx remains pending until the bounded attempt cap, then is
	// durably moved to the inbound DLQ before source acknowledgement.
	poison := map[string]any{"update_id": float64(105), "message": map[string]any{
		"chat": map[string]any{"id": float64(chatID)}, "text": "poison",
	}}
	if !h.proxy.ProcessUpdate(botID, poison) {
		t.Fatal("poison Update was not durably accepted")
	}
	poisonDelivery, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 105)
	if err != nil {
		t.Fatal(err)
	}
	h.Eventually(func() bool {
		d, err := h.store.GetInboundDelivery(context.Background(), poisonDelivery.DeliveryID)
		return err == nil && d.Status == models.InboundDLQ && d.AttemptCount == 2
	}, 2*time.Second, "non-2xx delivery exhausted into durable DLQ")

	// Exercise the actual fake Telegram getUpdates owner, not only direct
	// injection. The poller may advance only after Redis accepted update 106.
	polled := map[string]any{"update_id": float64(106), "message": map[string]any{
		"chat": map[string]any{"id": float64(chatID)}, "text": "from fake Telegram",
	}}
	h.fake.EnqueueUpdate(token, polled)
	h.proxy.Start()
	h.Eventually(func() bool {
		cfg, err := h.store.GetBotConfig(botID)
		return err == nil && cfg.Offset == 107
	}, 2*time.Second, "sole poller durably ingested fake Telegram Update")
	polledDelivery, err := h.store.GetInboundDeliveryByUpdate(context.Background(), botID, 106)
	if err != nil {
		t.Fatal(err)
	}
	h.Eventually(func() bool {
		d, err := h.store.GetInboundDelivery(context.Background(), polledDelivery.DeliveryID)
		return err == nil && d.Status == models.InboundSucceeded
	}, time.Second, "fake Telegram Update delivered")

	receiptsMu.Lock()
	received := append([]backendReceipt(nil), receipts...)
	receiptsMu.Unlock()
	if wrongBackendCalls.Load() != 0 {
		t.Fatal("route selection used content or ignored the receiving Bot Account")
	}
	var messageReceipts []backendReceipt
	for _, receipt := range received {
		id, _ := receipt.Update["update_id"].(float64)
		if int64(id) == 100 {
			messageReceipts = append(messageReceipts, receipt)
		}
		if receipt.Headers.Get("Authorization") != "Bearer "+backendCredential ||
			receipt.Headers.Get("X-Gateway-Route-Key") != "it_manage" ||
			receipt.Headers.Get("X-Gateway-Delivery-ID") == "" ||
			receipt.Headers.Get("X-Gateway-Attempt-ID") == "" {
			t.Fatalf("missing authenticated Gateway metadata: %v", receipt.Headers)
		}
		if _, err := strconv.Atoi(receipt.Headers.Get("X-Gateway-Attempt")); err != nil {
			t.Fatalf("invalid attempt metadata: %q", receipt.Headers.Get("X-Gateway-Attempt"))
		}
	}
	if len(messageReceipts) != 2 || !reflect.DeepEqual(messageReceipts[0].Update, messageUpdate) ||
		!reflect.DeepEqual(messageReceipts[1].Update, messageUpdate) {
		t.Fatalf("Backend did not receive the original Telegram Update on retry: %+v", messageReceipts)
	}

	// Finally prove append failure blocks the offset even though the SQLite row
	// was prepared. This models Redis outage without mutating the real service.
	broken := gateway.NewInbound(h.store, appendFailureQueue{}, nil, gateway.InboundConfig{})
	h.proxy.SetInboundGateway(broken)
	if h.proxy.ProcessUpdate(botID, map[string]any{"update_id": float64(200), "message": map[string]any{
		"chat": map[string]any{"id": float64(chatID)}, "text": "Redis unavailable",
	}}) {
		t.Fatal("offset advanced after Redis append failure")
	}
	cfg, _ := h.store.GetBotConfig(botID)
	if cfg.Offset != 107 {
		t.Fatalf("offset=%d after append failure, want 107", cfg.Offset)
	}
}

type appendFailureQueue struct{}

func (appendFailureQueue) AppendUnique(context.Context, string) (string, error) {
	return "", errors.New("queue unavailable")
}
func (appendFailureQueue) EnsureGroup(context.Context) error { return nil }
func (appendFailureQueue) ReadNew(context.Context, string, time.Duration, int64) ([]gateway.StreamMessage, error) {
	return nil, nil
}
func (appendFailureQueue) ClaimStale(context.Context, string, time.Duration, int64) ([]gateway.StreamMessage, error) {
	return nil, nil
}
func (appendFailureQueue) Ack(context.Context, string) error { return nil }
func (appendFailureQueue) MoveToDLQAndAck(context.Context, gateway.StreamMessage, string, int) error {
	return nil
}

func TestGatewayInboundExtractionNeverUsesContent(t *testing.T) {
	update := map[string]any{
		"update_id": float64(7),
		"callback_query": map[string]any{
			"id": "callback-id", "data": "chat_id=-999 route=evil",
			"message": map[string]any{"chat": map[string]any{"id": float64(-123)}},
		},
		"message": map[string]any{"chat": map[string]any{"id": float64(-999)}},
	}
	updateID, callbackID, chatID := gateway.ExtractInboundIdentity(update)
	if updateID != 7 || callbackID != "callback-id" || chatID != -123 {
		t.Fatalf("callback envelope extraction mismatch: %d %q %d", updateID, callbackID, chatID)
	}
	contentOnly := map[string]any{"update_id": float64(8), "inline_query": map[string]any{
		"query": strings.Repeat("chat_id=-123", 2),
	}}
	_, _, chatID = gateway.ExtractInboundIdentity(contentOnly)
	if chatID != 0 {
		t.Fatalf("content supplied a route chat: %d", chatID)
	}
}
