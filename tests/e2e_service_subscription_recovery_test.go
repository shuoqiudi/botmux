package tests

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
)

// Each durability case owns a Redis process and AOF directory, with no shared
// Redis instance or flush commands. AOF/always also survives a killed process.
func isolatedServiceRedis(t *testing.T) (string, func(), func()) {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for isolated AOF recovery acceptance")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	listener.Close()
	directory := t.TempDir()
	var process *exec.Cmd
	stop := func() {
		if process != nil {
			_ = process.Process.Kill()
			_ = process.Wait()
			process = nil
		}
	}
	start := func() {
		process = exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--dir", directory, "--appendonly", "yes", "--appendfsync", "always", "--save", "")
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
			if err == nil {
				connection.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("isolated Redis did not start")
	}
	start()
	t.Cleanup(stop)
	return address, stop, start
}

type interruptedServiceQueue struct {
	gateway.OutboundQueue
	appended    chan struct{}
	afterAppend bool
}

func (q *interruptedServiceQueue) Append(ctx context.Context, id string) (string, error) {
	if q.afterAppend {
		if _, err := q.OutboundQueue.Append(ctx, id); err != nil {
			return "", err
		}
	}
	select {
	case q.appended <- struct{}{}:
	default:
	}
	return "", errors.New("simulated connection lost")
}
func (q *interruptedServiceQueue) Read(ctx context.Context, _ string, _ int64) ([]gateway.QueueMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestE2E_ServiceSubscriptionDurableRecovery(t *testing.T) {
	for _, afterAppend := range []bool{false, true} {
		name := "before_redis_append"
		if afterAppend {
			name = "lost_append_response"
		}
		t.Run(name, func(t *testing.T) {
			address, stopRedis, startRedis := isolatedServiceRedis(t)
			path := filepath.Join(t.TempDir(), "service.db")
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
			t.Cleanup(func() { _ = f.h.store.Close() })
			subscription := f.subscribe(f.destination.ID, 1, 201)
			n := f.publish("durable", "Accepted before interruption")
			queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
			if err != nil {
				t.Fatal(err)
			}
			interrupted := &interruptedServiceQueue{OutboundQueue: queue, appended: make(chan struct{}, 1), afterAppend: afterAppend}
			worker := gateway.NewService(st, interrupted, gateway.NewTelegramHTTPClient(f.h.fake.URL()), "before-restart")
			worker.Start(context.Background())
			select {
			case <-interrupted.appended:
			case <-time.After(5 * time.Second):
				worker.Stop()
				t.Fatal("enqueue recovery did not run")
			}
			worker.Stop()
			queue.Close()
			// Neither cancellation nor target migration may rewrite this accepted plan.
			f.admin("DELETE", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions/"+itoa(subscription), map[string]any{"expected_revision": 2}, 200)
			f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(f.destination.ID), map[string]any{"name": "B", "bot_account_id": f.account.ID, "chat_id": -100940002, "expected_revision": 1}, 200)
			f.h.ts.Close()
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			stopRedis()
			startRedis()
			st, err = store.NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			f.h.store = st
			f.h.server = server.NewServer(st, nil)
			f.h.server.TgAPIBaseURL = f.h.fake.URL()
			f.h.ts = httptest.NewServer(f.h.server.BuildMux())
			t.Cleanup(f.h.ts.Close)
			queue, err = gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { queue.Close() })
			worker = gateway.NewService(st, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), "after-restart")
			worker.Start(context.Background())
			t.Cleanup(worker.Stop)
			result := f.await(n, "succeeded") // No producer replay is needed to recover.
			if result.Deliveries[0].ID != n.Deliveries[0].ID {
				t.Fatal("restart changed delivery")
			}
			requests := f.h.fake.RequestsFor("sendMessage")
			if len(requests) != 1 || !bytes.Contains(requests[0].body, []byte(`"chat_id":-100940001`)) {
				t.Fatalf("restart changed send: %+v", requests)
			}
			if replay := f.publish("durable", "Accepted before interruption"); replay.ID != n.ID || replay.Deliveries[0].ID != n.Deliveries[0].ID {
				t.Fatal("restart lost idempotency")
			}
		})
	}
}

type lostServiceReplayResponse struct{ gateway.OperationalQueue }

func (q lostServiceReplayResponse) ReplayFromDLQ(ctx context.Context, id string, generation int) (string, error) {
	if _, err := q.OperationalQueue.ReplayFromDLQ(ctx, id, generation); err != nil {
		return "", err
	}
	return "", errors.New("lost replay response")
}
func TestE2E_ServiceSubscriptionDeadLetterReplayRecovery(t *testing.T) {
	address, _, _ := isolatedServiceRedis(t)
	f := setupSubscription(t)
	subscription := f.subscribe(f.destination.ID, 1, 201)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"ok":false,"error_code":503}`)
	})
	queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	worker := gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), gateway.OutboundConfig{MaxAttempts: 2, BaseRetry: time.Millisecond, ClaimMinIdle: time.Millisecond})
	worker.Start(context.Background())
	n := f.publish("retry-exhausted", "Retry with original target")
	result := f.await(n, "failed")
	if result.Deliveries[0].AttemptCount != 2 || result.Deliveries[0].Status != "dead-lettered" {
		worker.Stop()
		t.Fatalf("retry bound: %+v", result)
	}
	worker.Stop()
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true,"result":{"message_id":991}}`) })
	f.admin("DELETE", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions/"+itoa(subscription), map[string]any{"expected_revision": 2}, 200)
	f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(f.destination.ID), map[string]any{"name": "B", "bot_account_id": f.account.ID, "chat_id": -100940002, "expected_revision": 1}, 200)
	f.h.server.SetGatewayOperations(gateway.NewOperations(f.h.store, lostServiceReplayResponse{queue}, nil, nil, nil, nil, nil))
	f.admin("POST", "/api/gateway/v1/ops/dlq/"+n.Deliveries[0].ID+"/replay", nil, 503)
	worker = gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), gateway.OutboundConfig{ClaimMinIdle: time.Millisecond})
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.await(n, "succeeded")
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 3 || !bytes.Contains(requests[2].body, []byte(`"chat_id":-100940001`)) {
		t.Fatalf("replay changed snapshot or duplicated: %+v", requests)
	}
}

func TestE2E_ServiceSubscriptionUncertainSendDoesNotRetry(t *testing.T) {
	address, _, _ := isolatedServiceRedis(t)
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			connection.Close()
		}
	})
	queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	worker := gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), gateway.OutboundConfig{ClaimMinIdle: time.Millisecond})
	worker.Start(context.Background())
	n := f.publish("uncertain", "Never resend blindly")
	result := f.await(n, "failed")
	worker.Stop()
	if result.Deliveries[0].Status != "reconciling" {
		t.Fatalf("uncertain: %+v", result)
	}
	worker = gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), gateway.OutboundConfig{ClaimMinIdle: time.Millisecond})
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	replay := f.publish("uncertain", "Never resend blindly")
	if replay.ID != n.ID || replay.Deliveries[0].Status != "reconciling" {
		t.Fatalf("uncertain replay: %+v", replay)
	}
	// More than one full Redis read/reclaim cycle; an uncertain outcome is terminal
	// for automatic sending, even after caller replay and worker restart.
	time.Sleep(1200 * time.Millisecond)
	if count := f.h.fake.RequestsCountFor("sendMessage"); count != 1 {
		t.Fatalf("uncertain message resent %d times", count)
	}
}

func TestE2E_ServiceSubscriptionRateLimitSurvivesWorkerRestart(t *testing.T) {
	address, _, _ := isolatedServiceRedis(t)
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}`)
	})
	queue, err := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })
	worker := gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), gateway.OutboundConfig{ClaimMinIdle: time.Millisecond})
	worker.Start(context.Background())
	n := f.publish("limited", "Rate limited")
	f.h.Eventually(func() bool {
		_, raw := serviceCall(t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		return bytes.Contains(raw, []byte(`"status":"retrying"`))
	}, 3*time.Second, "persistent rate limit")
	worker.Stop()
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true,"result":{"message_id":992}}`) })
	// A second delivery is also gated by the first delivery's persisted Bot limit.
	next := f.publish("after-limit", "Also wait for Bot")
	worker = gateway.NewServiceWithConfig(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), gateway.OutboundConfig{ClaimMinIdle: time.Millisecond})
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.await(n, "succeeded")
	f.await(next, "succeeded")
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 3 {
		t.Fatalf("rate limit sends: %d", len(requests))
	}
	for _, request := range requests[1:] {
		if request.timestamp.Sub(requests[0].timestamp) < 2*time.Second {
			t.Fatal("restart lost Bot rate limit")
		}
	}
}

func TestE2E_ServiceSubscriptionLegacyDeliveryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-subscription schema: route_id is mandatory, and retry/DLQ scheduling
	// columns may still be absent. Assertions remain at the management HTTP seam.
	_, err = db.Exec(`CREATE TABLE gateway_deliveries (
 id TEXT PRIMARY KEY,route_id INTEGER NOT NULL,route_revision INTEGER NOT NULL,
 workload_id INTEGER NOT NULL,direction TEXT NOT NULL,action TEXT NOT NULL,
 status TEXT NOT NULL,payload_hash TEXT NOT NULL,idempotency_key TEXT NOT NULL,
 attempt_count INTEGER NOT NULL DEFAULT 0,safe_error_class TEXT NOT NULL DEFAULT '',
 telegram_message_id INTEGER,created_at TEXT NOT NULL,accepted_at TEXT NOT NULL DEFAULT '',
 completed_at TEXT NOT NULL DEFAULT '',updated_at TEXT NOT NULL,
 UNIQUE(workload_id,route_id,idempotency_key),
 FOREIGN KEY(route_id) REFERENCES gateway_business_routes(id),
 FOREIGN KEY(workload_id) REFERENCES gateway_workloads(id));
 CREATE TABLE gateway_dlq (
 delivery_id TEXT PRIMARY KEY,route_id INTEGER NOT NULL,source_stream_id TEXT NOT NULL,
 error_class TEXT NOT NULL,attempt_count INTEGER NOT NULL,entered_at TEXT NOT NULL,
 FOREIGN KEY(delivery_id) REFERENCES gateway_deliveries(id),
 FOREIGN KEY(route_id) REFERENCES gateway_business_routes(id));
 CREATE TABLE gateway_delivery_payloads(delivery_id TEXT PRIMARY KEY,payload_json BLOB NOT NULL,FOREIGN KEY(delivery_id) REFERENCES gateway_deliveries(id));
 INSERT INTO gateway_deliveries(id,route_id,route_revision,workload_id,direction,action,status,payload_hash,idempotency_key,attempt_count,safe_error_class,created_at,updated_at)
 VALUES('legacy-delivery',1,1,1,'outbound','messages.send','dead-lettered','legacy-hash','legacy-key',2,'telegram_rejected','2026-09-01T00:00:00Z','2026-09-01T00:00:01Z');
 INSERT INTO gateway_dlq VALUES('legacy-delivery',1,'1-0','telegram_rejected',2,'2026-09-01T00:00:01Z');
 INSERT INTO gateway_delivery_payloads VALUES('legacy-delivery','{"kind":"message","message":{"text":"Legacy body"}}');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := setupSubscription(t, func(h *e2eHarness) {
		h.store = st
		h.server = server.NewServer(st, nil)
		h.server.TgAPIBaseURL = h.fake.URL()
		h.session = createTestAuth(t, st)
	})
	f.admin("POST", "/api/gateway/v1/routes", map[string]any{"route_key": "legacy", "display_name": "Legacy route", "bot_account_id": f.account.ID, "destination_id": f.destination.ID, "outbound_enabled": true, "enabled": true}, 201)
	f.h.server.SetGatewayOperations(gateway.NewOperations(st, nil, nil, nil, nil, nil, nil))
	raw := f.admin("GET", "/api/gateway/v1/ops/routes/legacy/deliveries/legacy-delivery", nil, 200)
	if !bytes.Contains(raw, []byte(`"attempt_count":2`)) || !bytes.Contains(raw, []byte(`"status":"dead-lettered"`)) {
		t.Fatalf("legacy delivery changed: %s", raw)
	}
	raw = f.admin("GET", "/api/gateway/v1/ops/dlq", nil, 200)
	if !bytes.Contains(raw, []byte("legacy-delivery")) || !bytes.Contains(raw, []byte("telegram_rejected")) {
		t.Fatalf("legacy DLQ lost: %s", raw)
	}
	// The upgraded store also accepts independent service deliveries.
	f.subscribe(f.destination.ID, 1, 201)
	if n := f.publish("upgraded", "New notification"); n.Status != "accepted" || len(n.Deliveries) != 1 {
		t.Fatalf("upgraded acceptance: %+v", n)
	}
}
