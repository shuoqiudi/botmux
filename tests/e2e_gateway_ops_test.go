package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

type opsQueue struct {
	mu       sync.Mutex
	next     int
	messages chan gateway.QueueMessage
	ids      map[string]string
	replays  map[string]string
	healthy  bool
}

func newOpsQueue() *opsQueue {
	return &opsQueue{messages: make(chan gateway.QueueMessage, 32), ids: map[string]string{}, replays: map[string]string{}, healthy: true}
}

func (q *opsQueue) nextID() string {
	q.next++
	return fmt.Sprintf("%d-0", q.next)
}

func (q *opsQueue) Append(_ context.Context, deliveryID string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.healthy {
		return "", errors.New("unavailable")
	}
	if id := q.ids[deliveryID]; id != "" {
		return id, nil
	}
	id := q.nextID()
	q.ids[deliveryID] = id
	q.messages <- gateway.QueueMessage{ID: id, DeliveryID: deliveryID}
	return id, nil
}

func (q *opsQueue) Read(ctx context.Context, _ string, _ int64) ([]gateway.QueueMessage, error) {
	select {
	case message := <-q.messages:
		return []gateway.QueueMessage{message}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *opsQueue) ClaimStale(context.Context, string, time.Duration, int64) ([]gateway.QueueMessage, error) {
	return nil, nil
}
func (q *opsQueue) Ack(context.Context, string) error { return nil }
func (q *opsQueue) MoveToDLQAndAck(context.Context, gateway.QueueMessage, gateway.DLQMetadata) error {
	return nil
}

func (q *opsQueue) InspectQueue(context.Context) (gateway.QueueObservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.healthy {
		return gateway.QueueObservation{}, errors.New("unavailable")
	}
	return gateway.QueueObservation{Available: true, Persistence: true, Depth: int64(len(q.messages)), CheckedAt: time.Now()}, nil
}

func (q *opsQueue) ReplayFromDLQ(_ context.Context, deliveryID string, generation int) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.healthy {
		return "", errors.New("unavailable")
	}
	key := fmt.Sprintf("%s:%d", deliveryID, generation)
	if id := q.replays[key]; id != "" {
		return id, nil
	}
	id := q.nextID()
	q.replays[key] = id
	q.ids[deliveryID] = id
	q.messages <- gateway.QueueMessage{ID: id, DeliveryID: deliveryID}
	return id, nil
}

func (q *opsQueue) setHealthy(value bool) {
	q.mu.Lock()
	q.healthy = value
	q.mu.Unlock()
}

func TestE2E_GatewayOperationsSerial(t *testing.T) {
	h := setupE2E(t)
	const token = "920001:ops-telegram-secret"
	const backendToken = "ops-backend-secret"
	const chatID int64 = -100920001
	h.fake.RegisterBot(token, "ops_bot", 920001)
	h.fake.RegisterChat(token, chatID, "Ops destination")
	var telegramAuthHealthy, destinationHealthy atomic.Bool
	telegramAuthHealthy.Store(true)
	destinationHealthy.Store(true)
	h.fake.SetHandler("getMe", func(w http.ResponseWriter, _ *http.Request) {
		if !telegramAuthHealthy.Load() {
			h.fake.writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error_code": 401})
			return
		}
		h.fake.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"id": 920001, "username": "ops_bot"}})
	})
	h.fake.SetHandler("getChat", func(w http.ResponseWriter, _ *http.Request) {
		if !destinationHealthy.Load() {
			h.fake.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error_code": 400})
			return
		}
		h.fake.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"id": chatID, "title": "Ops destination"}})
	})
	nativeBotID := h.AddBot(models.BotConfig{Name: "native ops", Token: token, ManageEnabled: true})
	if nativeBotID == 0 {
		t.Fatal("native bot not created")
	}

	var backendHealthy atomic.Bool
	backendHealthy.Store(true)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Method != http.MethodGet {
			t.Errorf("backend health used delivery endpoint: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+backendToken {
			t.Error("backend health credential missing")
		}
		if !backendHealthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	accountID, err := h.store.AddBotAccount(models.BotAccount{Name: "Ops Bot", Username: "ops_bot", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	destinationID, err := h.store.AddTelegramDestination(models.TelegramDestination{Name: "Ops target", BotAccountID: accountID, ChatID: chatID, Status: "active", ValidatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	routeID, err := h.store.AddBusinessRoute(models.BusinessRoute{
		RouteKey: "ops", DisplayName: "Operations", BotAccountID: accountID, DestinationID: destinationID,
		InboundEnabled: true, InboundBackendURL: backend.URL + "/updates", InboundBackendHealthURL: backend.URL + "/health",
		InboundBackendToken: backendToken, OutboundEnabled: true, Enabled: true, Status: "active", LastValidatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	workloadID, err := h.store.CreateGatewayWorkload("ops-fixture", "primary", auth.HashAPIKey("ops-workload-secret"))
	if err != nil {
		t.Fatal(err)
	}

	queue := newOpsQueue()
	telegram := gateway.NewTelegramHTTPClient(h.fake.URL())
	service := gateway.NewServiceWithConfig(h.store, queue, telegram, gateway.OutboundConfig{Consumer: "ops-worker", ClaimMinIdle: time.Millisecond})
	operations := gateway.NewOperations(h.store, queue, nil, service, nil, telegram, nil)
	h.server.SetGatewayService(service)
	h.server.SetGatewayOperations(operations)
	h.ts = httptest.NewServer(h.server.BuildMux())
	t.Cleanup(h.ts.Close)

	adminCall := func(method, path string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, h.ts.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
		resp, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	status, _ := adminCall(http.MethodGet, "/api/gateway/v1/ops/health")
	if status != http.StatusOK {
		t.Fatalf("ops health status=%d", status)
	}
	resp, err := http.Get(h.ts.URL + "/api/gateway/v1/ops/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unprotected operations status=%d", resp.StatusCode)
	}
	passwordHash, err := auth.HashPassword("ops-password")
	if err != nil {
		t.Fatal(err)
	}
	operatorID, err := h.store.CreateUser("ops-operator", passwordHash, "Operator", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSession("ops-operator-session", operatorID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	userID, err := h.store.CreateUser("ops-user", passwordHash, "User", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSession("ops-user-session", userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	roleCall := func(session string) int {
		req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/gateway/v1/ops/health", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	if roleCall("ops-operator-session") != http.StatusOK || roleCall("ops-user-session") != http.StatusForbidden {
		t.Fatal("Gateway operations role boundary is not enforced")
	}

	payload := []byte(`{"kind":"message","message":{"text":"internal ops payload"}}`)
	created, _, err := h.store.CreateGatewayOutboundDelivery(context.Background(), models.GatewayDeliveryCreate{
		ID: "ops-accepted", RouteID: routeID, RouteRevision: 1, WorkloadID: workloadID, Action: models.GatewayActionMessagesSend,
		PayloadHash: "safe-hash-1", IdempotencyKey: "ops-accepted", PayloadJSON: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Append(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkGatewayDeliveryAccepted(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}

	status, metricsBody := adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
	if status != http.StatusOK {
		t.Fatalf("route metrics status=%d body=%s", status, metricsBody)
	}
	var metrics models.GatewayRouteMetrics
	if err := json.Unmarshal(metricsBody, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.Counts.Accepted != 1 || metrics.QueueDepth != 1 || metrics.Components.BotAuthentication.Status != "healthy" ||
		metrics.Components.DestinationValidation.Status != "healthy" || metrics.Components.BackendHealth.Status != "healthy" ||
		metrics.Components.Redis.Status != "healthy" || metrics.Components.OutboundWorker.Status != "stopped" ||
		metrics.Components.TelegramPolling.Status != "unhealthy" {
		t.Fatalf("independent initial component/metric state: %+v", metrics)
	}
	h.proxy.Start()
	h.Eventually(func() bool {
		_, body := adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
		var current models.GatewayRouteMetrics
		return json.Unmarshal(body, &current) == nil && current.Components.TelegramPolling.Status == "healthy"
	}, 2*time.Second, "Telegram poller component recovers independently")

	backendHealthy.Store(false)
	_, metricsBody = adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
	_ = json.Unmarshal(metricsBody, &metrics)
	if metrics.Components.BackendHealth.Status != "unhealthy" || metrics.Components.BotAuthentication.Status != "healthy" || metrics.Components.Redis.Status != "healthy" {
		t.Fatalf("backend fault leaked into independent components: %+v", metrics.Components)
	}
	backendHealthy.Store(true)
	telegramAuthHealthy.Store(false)
	_, metricsBody = adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
	_ = json.Unmarshal(metricsBody, &metrics)
	if metrics.Components.BotAuthentication.Status != "unhealthy" || metrics.Components.DestinationValidation.Status != "unknown" || metrics.Components.BackendHealth.Status != "healthy" {
		t.Fatalf("Telegram auth fault leaked into independent components: %+v", metrics.Components)
	}
	telegramAuthHealthy.Store(true)
	destinationHealthy.Store(false)
	_, metricsBody = adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
	_ = json.Unmarshal(metricsBody, &metrics)
	if metrics.Components.BotAuthentication.Status != "healthy" || metrics.Components.DestinationValidation.Status != "unhealthy" || metrics.Components.BackendHealth.Status != "healthy" {
		t.Fatalf("destination fault leaked into independent components: %+v", metrics.Components)
	}
	destinationHealthy.Store(true)
	queue.setHealthy(false)
	_, metricsBody = adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
	_ = json.Unmarshal(metricsBody, &metrics)
	if metrics.Components.Redis.Status != "unhealthy" || metrics.Components.BackendHealth.Status != "healthy" {
		t.Fatalf("Redis fault leaked into backend component: %+v", metrics.Components)
	}
	queue.setHealthy(true)

	service.Start(context.Background())
	t.Cleanup(service.Stop)
	h.Eventually(func() bool {
		delivery, getErr := h.store.GetGatewayDelivery(context.Background(), routeID, created.ID)
		return getErr == nil && delivery.Status == "succeeded"
	}, 2*time.Second, "accepted delivery succeeds after worker recovery")
	_, metricsBody = adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops")
	_ = json.Unmarshal(metricsBody, &metrics)
	if metrics.Counts.Accepted != 0 || metrics.Counts.Succeeded != 1 || metrics.QueueDepth != 0 || metrics.Components.OutboundWorker.Status != "healthy" {
		t.Fatalf("success metrics did not transition: %+v", metrics)
	}

	createDead := func(id string) {
		t.Helper()
		_, _, createErr := h.store.CreateGatewayOutboundDelivery(context.Background(), models.GatewayDeliveryCreate{
			ID: id, RouteID: routeID, RouteRevision: 1, WorkloadID: workloadID, Action: models.GatewayActionMessagesSend,
			PayloadHash: "safe-" + id, IdempotencyKey: id, PayloadJSON: payload,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if createErr = h.store.MarkGatewayDeliveryAccepted(context.Background(), id); createErr != nil {
			t.Fatal(createErr)
		}
		if createErr = h.store.MarkGatewayDeliveryDeadLettered(context.Background(), id, "", "source-"+id, "telegram_unavailable", 2); createErr != nil {
			t.Fatal(createErr)
		}
	}
	createDead("ops-replay")
	createDead("ops-discard")
	status, dlqBody := adminCall(http.MethodGet, "/api/gateway/v1/ops/dlq")
	if status != http.StatusOK || !bytes.Contains(dlqBody, []byte("ops-replay")) || !bytes.Contains(dlqBody, []byte("ops-discard")) {
		t.Fatalf("DLQ list status=%d body=%s", status, dlqBody)
	}
	for _, forbidden := range []string{token, backendToken, strconv.FormatInt(chatID, 10), "internal ops payload", backend.URL} {
		if bytes.Contains(metricsBody, []byte(forbidden)) || bytes.Contains(dlqBody, []byte(forbidden)) {
			t.Fatalf("ops response exposed protected data %q", forbidden)
		}
	}

	status, detailBody := adminCall(http.MethodGet, "/api/gateway/v1/ops/routes/ops/deliveries/ops-replay")
	if status != http.StatusOK || !bytes.Contains(detailBody, []byte(`"error_class":"telegram_unavailable"`)) {
		t.Fatalf("safe delivery detail status=%d body=%s", status, detailBody)
	}
	if bytes.Contains(detailBody, payload) || bytes.Contains(detailBody, []byte(token)) || bytes.Contains(detailBody, []byte(strconv.FormatInt(chatID, 10))) {
		t.Fatalf("delivery detail exposed payload or physical destination: %s", detailBody)
	}

	status, replayBody := adminCall(http.MethodPost, "/api/gateway/v1/ops/dlq/ops-replay/replay")
	if status != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", status, replayBody)
	}
	h.Eventually(func() bool {
		delivery, getErr := h.store.GetGatewayDelivery(context.Background(), routeID, "ops-replay")
		return getErr == nil && delivery.Status == "succeeded"
	}, 2*time.Second, "replayed delivery succeeds")
	sendsAfterReplay := h.fake.RequestsCountFor("sendMessage")
	status, replayBody = adminCall(http.MethodPost, "/api/gateway/v1/ops/dlq/ops-replay/replay")
	if status != http.StatusOK || !bytes.Contains(replayBody, []byte(`"idempotent":true`)) {
		t.Fatalf("idempotent replay status=%d body=%s", status, replayBody)
	}
	time.Sleep(25 * time.Millisecond)
	if h.fake.RequestsCountFor("sendMessage") != sendsAfterReplay {
		t.Fatal("idempotent replay enqueued a duplicate Telegram send")
	}

	status, discardBody := adminCall(http.MethodPost, "/api/gateway/v1/ops/dlq/ops-discard/discard")
	if status != http.StatusOK {
		t.Fatalf("discard status=%d body=%s", status, discardBody)
	}
	status, discardBody = adminCall(http.MethodPost, "/api/gateway/v1/ops/dlq/ops-discard/discard")
	if status != http.StatusOK || !bytes.Contains(discardBody, []byte(`"idempotent":true`)) {
		t.Fatalf("idempotent discard status=%d body=%s", status, discardBody)
	}
	discarded, err := h.store.GetGatewayDelivery(context.Background(), routeID, "ops-discard")
	if err != nil || discarded.Status != "discarded" {
		t.Fatalf("discard state=%+v err=%v", discarded, err)
	}

	status, audit := adminCall(http.MethodGet, "/api/gateway/v1/audit?route_key=ops&limit=100")
	if status != http.StatusOK || bytes.Count(audit, []byte(`"dlq.replay"`)) != 1 || bytes.Count(audit, []byte(`"dlq.discard"`)) != 1 {
		t.Fatalf("DLQ audit is missing or duplicated: %s", audit)
	}
	for _, forbidden := range []string{token, backendToken, strconv.FormatInt(chatID, 10), "internal ops payload", backend.URL} {
		if bytes.Contains(audit, []byte(forbidden)) {
			t.Fatalf("audit exposed protected data %q", forbidden)
		}
	}
}
