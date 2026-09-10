package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
)

// Interrupt a real Redis connection after one target was fully enqueued. The
// second append can reach Redis without its response reaching the dispatcher.
type partialServiceQueue struct {
	gateway.OutboundQueue
	calls       int
	afterAppend bool
	interrupted chan struct{}
}

func (q *partialServiceQueue) Append(ctx context.Context, id string) (string, error) {
	q.calls++
	if q.calls == 1 {
		return q.OutboundQueue.Append(ctx, id)
	}
	if q.calls == 2 {
		if q.afterAppend {
			if _, err := q.OutboundQueue.Append(ctx, id); err != nil {
				return "", err
			}
		}
		close(q.interrupted)
	}
	<-ctx.Done()
	return "", ctx.Err()
}
func (q *partialServiceQueue) Read(ctx context.Context, _ string, _ int64) ([]gateway.QueueMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestE2E_ServiceMultiTargetPartialEnqueueRecovery(t *testing.T) {
	for _, afterAppend := range []bool{false, true} {
		t.Run(fmt.Sprintf("second_append_reached_redis_%t", afterAppend), func(t *testing.T) {
			address, stopRedis, startRedis := isolatedServiceRedis(t)
			path := filepath.Join(t.TempDir(), "multi.db")
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
			a := f.subscribe(f.destination.ID, 1, 201)
			b := f.destinationFor(f.account.ID, -100940002, "B")
			f.subscribe(b.ID, 2, 201)
			other := f.secondBot()
			c := f.destinationFor(other.ID, -100940003, "C")
			f.subscribe(c.ID, 3, 201)
			n := f.publish("partial", "Complete durable plan")
			queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
			if err != nil {
				t.Fatal(err)
			}
			interrupted := &partialServiceQueue{OutboundQueue: queue, afterAppend: afterAppend, interrupted: make(chan struct{})}
			worker := gateway.NewService(st, interrupted, gateway.NewTelegramHTTPClient(f.h.fake.URL()), "partial")
			worker.Start(context.Background())
			select {
			case <-interrupted.interrupted:
			case <-time.After(5 * time.Second):
				worker.Stop()
				t.Fatal("no partial enqueue")
			}
			worker.Stop()
			queue.Close()
			f.admin("DELETE", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions/"+itoa(a), map[string]any{"expected_revision": 4}, 200)
			f.h.fake.RegisterChat("940001:subscription-secret", -100940004, "D")
			d := f.destinationFor(f.account.ID, -100940004, "D")
			f.subscribe(d.ID, 5, 201)
			f.h.ts.Close()
			st.Close()
			stopRedis()
			// SQLite accepts a new complete plan while Redis is actually unavailable.
			st, err = store.NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			f.h.store = st
			f.h.server = server.NewServer(st, nil)
			f.h.server.TgAPIBaseURL = f.h.fake.URL()
			f.h.ts = httptest.NewServer(f.h.server.BuildMux())
			t.Cleanup(f.h.ts.Close)
			fresh := f.publish("during-outage", "New subscribers during Redis outage")
			if fresh.SubscriptionCount != 3 {
				t.Fatalf("outage lost plan: %+v", fresh)
			}
			replay := f.publish("partial", "Complete durable plan")
			if replay.ID != n.ID || len(replay.Deliveries) != 3 {
				t.Fatalf("old request expanded: %+v", replay)
			}
			for i := range n.Deliveries {
				if replay.Deliveries[i].ID != n.Deliveries[i].ID {
					t.Fatal("changed child identity")
				}
			}
			startRedis()
			queue, err = gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { queue.Close() })
			// Duplicate publication into Redis must not create another Telegram send.
			for _, d := range n.Deliveries {
				if _, err := queue.Append(context.Background(), d.ID); err != nil {
					t.Fatal(err)
				}
			}
			worker = gateway.NewService(st, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), "restored")
			worker.Start(context.Background())
			t.Cleanup(worker.Stop)
			f.await(n, "succeeded")
			f.await(fresh, "succeeded")
			requests := f.h.fake.RequestsFor("sendMessage")
			if len(requests) != 6 {
				t.Fatalf("lost or duplicate targets: %d", len(requests))
			}
			got := map[string]map[int64]bool{}
			for _, r := range requests {
				var sent struct {
					Text   string `json:"text"`
					ChatID int64  `json:"chat_id"`
				}
				json.Unmarshal(r.body, &sent)
				if got[sent.Text] == nil {
					got[sent.Text] = map[int64]bool{}
				}
				if got[sent.Text][sent.ChatID] {
					t.Fatal("duplicate recipient")
				}
				got[sent.Text][sent.ChatID] = true
			}
			old := got["Complete durable plan"]
			newSet := got["New subscribers during Redis outage"]
			if !old[-100940001] || old[-100940004] || newSet[-100940001] || !newSet[-100940004] {
				t.Fatalf("snapshot rewritten: %+v", got)
			}
		})
	}
}

func TestE2E_ServiceMultiTargetLostReceiptAndConcurrentEdit(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	b := f.destinationFor(f.account.ID, -100940002, "B")
	mux := f.h.server.BuildMux()
	committed := make(chan models.ServiceNotification, 1)
	var once sync.Once
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "lost-receipt" {
			lost := false
			once.Do(func() { lost = true })
			if lost {
				record := httptest.NewRecorder()
				mux.ServeHTTP(record, r)
				var n models.ServiceNotification
				json.Unmarshal(record.Body.Bytes(), &n)
				committed <- n
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	defer front.Close()
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		request, _ := http.NewRequest("POST", front.URL+"/api/v1/services/notifications", bytes.NewBufferString(`{"fingerprint":"dns","text":"Lost response"}`))
		request.Header.Set("Authorization", "Bearer subscriber-secret")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "lost-receipt")
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	close(start)
	f.subscribe(b.ID, 2, 201)
	if err := <-done; err == nil {
		t.Fatal("expected lost HTTP response")
	}
	original := <-committed
	replay := f.publish("lost-receipt", "Lost response")
	if original.ID == "" || replay.ID != original.ID || replay.SubscriptionCount != original.SubscriptionCount || len(replay.Deliveries) != len(original.Deliveries) {
		t.Fatalf("lost response duplicated receipt: %+v %+v", original, replay)
	}
	if original.SubscriptionCount != 1 && original.SubscriptionCount != 2 {
		t.Fatalf("partial snapshot: %+v", original)
	}
	for i := range original.Deliveries {
		if original.Deliveries[i].ID != replay.Deliveries[i].ID {
			t.Fatal("lost response duplicated delivery")
		}
	}
	worker := gateway.NewService(f.h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(f.h.fake.URL()), "lost-receipt")
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.await(replay, "succeeded")
	if f.h.fake.RequestsCountFor("sendMessage") != original.SubscriptionCount {
		t.Fatal("wrong recipient count after replay")
	}
}

func TestE2E_ServiceMultiTargetConcurrentConfigurationSnapshot(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	b := f.destinationFor(f.account.ID, -100940002, "B")
	f.subscribe(b.ID, 2, 201)
	f.h.fake.RegisterBot("940009:replacement-secret", "replacement", 940009)
	f.h.fake.RegisterChat("940009:replacement-secret", -100940001, "A")
	f.h.fake.RegisterChat("940009:replacement-secret", -100940002, "B")
	// One account edit changes both subscribers together. Every receipt must
	// contain either both old-Bot targets or both new-Bot targets, never a mix.
	for round := 0; round < 8; round++ {
		start := make(chan struct{})
		done := make(chan models.ServiceNotification, 1)
		go func() { <-start; done <- f.publish(fmt.Sprintf("concurrent-%d", round), "Concurrent snapshot") }()
		token := "940009:replacement-secret"
		if round%2 == 1 {
			token = "940001:subscription-secret"
		}
		close(start)
		f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID), map[string]any{"name": "Subscriber", "token": token, "expected_revision": round + 1}, 200)
		n := <-done
		var history struct {
			Notifications []models.ServiceNotification `json:"notifications"`
		}
		json.Unmarshal(f.admin("GET", "/api/gateway/v1/services/"+itoa(f.serviceID), nil, 200), &history)
		var found bool
		for _, item := range history.Notifications {
			if item.ID != n.ID {
				continue
			}
			found = true
			if item.SubscriptionCount != 2 || len(item.Deliveries) != 2 {
				t.Fatalf("incomplete plan: %+v", item)
			}
			left, right := item.Deliveries[0], item.Deliveries[1]
			if left.TelegramBotID != right.TelegramBotID || (left.TelegramBotID != 940001 && left.TelegramBotID != 940009) || left.ChatID != -100940001 || right.ChatID != -100940002 {
				t.Fatalf("mixed configuration versions: %+v", item)
			}
		}
		if !found {
			t.Fatal("receipt missing from history")
		}
	}
	// A replacement Bot cannot receive any accepted work for the original Bot;
	// the sibling on an unrelated Bot continues despite that identity failure.
	other := f.secondBot()
	c := f.destinationFor(other.ID, -100940003, "C")
	f.subscribe(c.ID, 3, 201)
	n := f.publish("replacement-isolation", "Pinned original Bot")
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID), map[string]any{"name": "Replacement", "token": "940009:replacement-secret", "expected_revision": 9}, 200)
	worker := gateway.NewService(f.h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(f.h.fake.URL()), "replacement-isolation")
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.h.Eventually(func() bool {
		_, raw := serviceCall(t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		var result models.ServiceNotification
		json.Unmarshal(raw, &result)
		return len(result.Deliveries) == 3 && result.Deliveries[0].ErrorClass == "service_bot_identity_changed" && result.Deliveries[1].ErrorClass == "service_bot_identity_changed" && result.Deliveries[2].Status == "succeeded"
	}, 5*time.Second, "Bot replacement isolated from sibling")
}

func TestE2E_ServiceMultiTargetInterruptedDispatch(t *testing.T) {
	address, stopRedis, startRedis := isolatedServiceRedis(t)
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	b := f.destinationFor(f.account.ID, -100940002, "B")
	f.subscribe(b.ID, 2, 201)
	dispatched := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChatID int64 `json:"chat_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.ChatID == -100940001 {
			dispatched <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":998}}`)
	})
	queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	config := gateway.OutboundConfig{ClaimMinIdle: time.Millisecond}
	worker := gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), config)
	worker.Start(context.Background())
	n := f.publish("interrupted-dispatch", "Uncertain A, independent B")
	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		worker.Stop()
		t.Fatal("no dispatch")
	}
	worker.Stop()
	queue.Close()
	stopRedis()
	startRedis()
	queue, err = gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	worker = gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), config)
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.h.Eventually(func() bool {
		_, raw := serviceCall(t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		var result models.ServiceNotification
		json.Unmarshal(raw, &result)
		return len(result.Deliveries) == 2 && result.Deliveries[0].Status == "reconciling" && result.Deliveries[1].Status == "succeeded" && result.DeliverySummary.Status == "failed"
	}, 5*time.Second, "reconcile interrupted target and send sibling")
	replay := f.publish("interrupted-dispatch", "Uncertain A, independent B")
	if replay.ID != n.ID || replay.Deliveries[0].Status != "reconciling" {
		t.Fatalf("uncertainty lost: %+v", replay)
	}
	worker.Stop()
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 2 {
		t.Fatalf("blind resend after dispatch: %d", len(requests))
	}
}
