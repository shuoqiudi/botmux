package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

func (f *subscriptionFixture) destinationFor(account int64, chat int64, name string) models.TelegramDestination {
	f.t.Helper()
	var d models.TelegramDestination
	if err := json.Unmarshal(f.admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": name, "bot_account_id": account, "chat_id": chat}, 201), &d); err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *subscriptionFixture) secondBot() models.BotAccount {
	f.t.Helper()
	f.h.fake.RegisterBot("950002:other-secret", "other_subscriber", 950002)
	f.h.fake.RegisterChat("950002:other-secret", -100940001, "A")
	f.h.fake.RegisterChat("950002:other-secret", -100940003, "C")
	var a models.BotAccount
	if err := json.Unmarshal(f.admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Other Bot", "token": "950002:other-secret"}, 201), &a); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestE2E_ServiceMultiTargetIdentities(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	b := f.destinationFor(f.account.ID, -100940002, "B")
	f.subscribe(b.ID, 2, 201)
	other := f.secondBot()
	c := f.destinationFor(other.ID, -100940003, "C")
	f.subscribe(c.ID, 3, 201)
	sameChat := f.destinationFor(other.ID, -100940001, "Other Bot A")
	f.subscribe(sameChat.ID, 4, 201)
	n := f.publish("four-targets", "One request, four recipients")
	if n.SubscriptionCount != 4 || len(n.Deliveries) != 4 {
		t.Fatalf("incomplete plan: %+v", n)
	}
	code, raw := serviceCall(t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
	if code != 200 || bytes.Contains(raw, []byte(`"chat_id"`)) {
		t.Fatalf("producer target disclosure: %d %s", code, raw)
	}
	raw = f.admin("GET", "/api/gateway/v1/services/"+itoa(f.serviceID), nil, 200)
	var history struct {
		Notifications []json.RawMessage `json:"notifications"`
	}
	json.Unmarshal(raw, &history)
	raw = history.Notifications[0]
	var detail struct {
		Deliveries []struct {
			ID             string `json:"delivery_id"`
			SubscriptionID int64  `json:"subscription_id"`
			BotID          int64  `json:"telegram_bot_id"`
			ChatID         int64  `json:"chat_id"`
		} `json:"deliveries"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if code != 200 || len(detail.Deliveries) != 4 {
		t.Fatalf("detail: %d %s", code, raw)
	}
	want := map[[2]int64]bool{{940001, -100940001}: true, {940001, -100940002}: true, {950002, -100940003}: true, {950002, -100940001}: true}
	for _, d := range detail.Deliveries {
		key := [2]int64{d.BotID, d.ChatID}
		if !want[key] || d.SubscriptionID == 0 {
			t.Fatalf("missing/duplicate snapshot target: %s", raw)
		}
		delete(want, key)
	}
	worker := gateway.NewService(f.h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(f.h.fake.URL()), "multi")
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	result := f.await(n, "succeeded")
	if result.DeliverySummary.Succeeded != 4 {
		t.Fatalf("summary: %+v", result)
	}
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 4 {
		t.Fatalf("sends: %d", len(requests))
	}
	targets := map[string]bool{}
	for _, r := range requests {
		var body struct {
			ChatID int64 `json:"chat_id"`
		}
		json.Unmarshal(r.body, &body)
		key := fmt.Sprintf("%s/%d", r.token, body.ChatID)
		if targets[key] {
			t.Fatalf("duplicate send %s", key)
		}
		targets[key] = true
	}
	// Management history exposes the same immutable targets as the producer query.
	raw = f.admin("GET", "/api/gateway/v1/services/"+itoa(f.serviceID), nil, 200)
	if !bytes.Contains(raw, []byte(`"telegram_bot_id":950002`)) {
		t.Fatalf("history omitted targets: %s", raw)
	}
}

func TestE2E_ServiceMultiTargetFinal429GatesSameBot(t *testing.T) {
	address, stopRedis, startRedis := isolatedServiceRedis(t)
	path := filepath.Join(t.TempDir(), "rate.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f := setupSubscription(t, func(h *e2eHarness) {
		h.store = st
		h.server = server.NewServer(st, nil)
		h.server.TgAPIBaseURL = h.fake.URL()
		h.session = createTestAuth(t, st)
	})
	t.Cleanup(func() { f.h.store.Close() })
	f.subscribe(f.destination.ID, 1, 201)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":3}}`)
	})
	queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	config := gateway.OutboundConfig{MaxAttempts: 1, ClaimMinIdle: time.Millisecond}
	worker := gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), config)
	worker.Start(context.Background())
	first := f.publish("last-429", "Exhausted but Bot still limited")
	f.await(first, "failed")
	worker.Stop()
	queue.Close()
	stopRedis()
	f.h.ts.Close()
	st.Close()
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f.h.store = st
	f.h.server = server.NewServer(st, nil)
	f.h.server.TgAPIBaseURL = f.h.fake.URL()
	f.h.ts = httptest.NewServer(f.h.server.BuildMux())
	t.Cleanup(f.h.ts.Close)
	startRedis()
	queue, err = gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	var alias models.BotAccount
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Same Bot alias", "token": "940001:subscription-secret"}, 201), &alias)
	b := f.destinationFor(alias.ID, -100940002, "B")
	f.subscribe(b.ID, 2, 201)
	other := f.secondBot()
	c := f.destinationFor(other.ID, -100940003, "C")
	f.subscribe(c.ID, 3, 201)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true,"result":{"message_id":995}}`) })
	n := f.publish("after-final-429", "Other Bot continues")
	worker = gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), config)
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.await(n, "succeeded")
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 4 {
		t.Fatalf("sends: %d", len(requests))
	}
	for _, r := range requests[1:] {
		delay := r.timestamp.Sub(requests[0].timestamp)
		if r.token == "940001:subscription-secret" && delay < 3*time.Second {
			t.Fatalf("final 429 lost Bot gate: %s", delay)
		}
		if r.token == "950002:other-secret" && delay >= 3*time.Second {
			t.Fatalf("unrelated Bot blocked: %s", delay)
		}
	}
}

func TestE2E_ServiceMultiTargetFailureIsolationAndReplay(t *testing.T) {
	address, _, _ := isolatedServiceRedis(t)
	f := setupSubscription(t)
	firstSub := f.subscribe(f.destination.ID, 1, 201)
	b := f.destinationFor(f.account.ID, -100940002, "B")
	f.subscribe(b.ID, 2, 201)
	other := f.secondBot()
	c := f.destinationFor(other.ID, -100940003, "C")
	f.subscribe(c.ID, 3, 201)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChatID int64 `json:"chat_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		switch body.ChatID {
		case -100940001:
			w.WriteHeader(503)
			fmt.Fprint(w, `{"ok":false,"error_code":503}`)
		case -100940002:
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				connection.Close()
			}
		default:
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":996}}`)
		}
	})
	queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	config := gateway.OutboundConfig{MaxAttempts: 2, BaseRetry: time.Millisecond, ClaimMinIdle: time.Millisecond}
	worker := gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), config)
	f.h.server.SetGatewayOperations(gateway.NewOperations(f.h.store, queue, nil, worker, nil, nil, nil))
	worker.Start(context.Background())
	t.Cleanup(func() { worker.Stop() })
	n := f.publish("mixed-results", "Keep targets independent")
	var result models.ServiceNotification
	f.h.Eventually(func() bool {
		_, raw := serviceCall(t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		json.Unmarshal(raw, &result)
		return len(result.Deliveries) == 3 && result.Deliveries[0].Status == "dead-lettered" && result.Deliveries[1].Status == "reconciling" && result.Deliveries[2].Status == "succeeded"
	}, 5*time.Second, "all target outcomes")
	worker.Stop()
	if result.DeliverySummary.Status != "failed" || result.DeliverySummary.Succeeded != 1 || result.Deliveries[0].AttemptCount != 2 {
		t.Fatalf("partial success misreported: %+v", result)
	}
	f.admin("DELETE", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions/"+itoa(firstSub), map[string]any{"expected_revision": 4}, 200)
	f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(f.destination.ID), map[string]any{"name": "Moved A", "bot_account_id": f.account.ID, "chat_id": -100940004, "expected_revision": 1}, 422) // Telegram must validate an unknown chat.
	f.h.fake.RegisterChat("940001:subscription-secret", -100940004, "D")
	f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(f.destination.ID), map[string]any{"name": "Moved A", "bot_account_id": f.account.ID, "chat_id": -100940004, "expected_revision": 1}, 200)
	const rotated = "940001:multi-rotated"
	f.h.fake.RegisterBot(rotated, "subscriber", 940001)
	f.h.fake.RegisterChat(rotated, -100940001, "A")
	f.h.fake.RegisterChat(rotated, -100940002, "B")
	f.h.fake.RegisterChat(rotated, -100940004, "D")
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID), map[string]any{"name": "Subscriber", "token": rotated, "expected_revision": 1}, 200)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true,"result":{"message_id":997}}`) })
	// A regular user may neither replay nor discard a target's dead letter.
	adminSession := f.h.session
	uid, err := f.h.store.CreateUser("multi-user", "unused", "User", "user")
	if err != nil {
		t.Fatal(err)
	}
	f.h.store.CreateSession("multi-user", uid, time.Now().Add(time.Hour))
	f.h.session = "multi-user"
	for _, action := range []string{"replay", "discard"} {
		f.admin("POST", "/api/gateway/v1/ops/dlq/"+n.Deliveries[0].ID+"/"+action, nil, 403)
	}
	f.h.session = adminSession
	// Lose the Redis replay acknowledgement; the outbox must finish this
	// generation without a second operator action or sibling replay.
	f.h.server.SetGatewayOperations(gateway.NewOperations(f.h.store, lostServiceReplayResponse{queue}, nil, nil, nil, nil, nil))
	f.admin("POST", "/api/gateway/v1/ops/dlq/"+n.Deliveries[0].ID+"/replay", nil, 503)
	worker = gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), config)
	worker.Start(context.Background())
	f.h.Eventually(func() bool {
		_, raw := serviceCall(t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		json.Unmarshal(raw, &result)
		return result.DeliverySummary.Succeeded == 2
	}, 5*time.Second, "single target replay")
	worker.Stop()
	if result.DeliverySummary.Status != "failed" || result.Deliveries[1].Status != "reconciling" {
		t.Fatalf("uncertainty hidden: %+v", result)
	}
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 5 || requests[4].token != rotated || !bytes.Contains(requests[4].body, []byte(`"chat_id":-100940001`)) {
		t.Fatalf("replay resent siblings or changed target: %+v", requests)
	}
	replay := f.publish("mixed-results", "Keep targets independent")
	if replay.ID != n.ID || len(replay.Deliveries) != 3 {
		t.Fatalf("replay expanded changed subscriptions: %+v", replay)
	}
	raw := f.admin("GET", "/api/gateway/v1/audit", nil, 200)
	if !bytes.Contains(raw, []byte("dlq.replay")) || !bytes.Contains(raw, []byte(n.Deliveries[0].ID)) {
		t.Fatalf("missing replay audit: %s", raw)
	}
}
