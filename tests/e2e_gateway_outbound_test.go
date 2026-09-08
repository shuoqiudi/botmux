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
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

type memoryOutboundQueue struct {
	mu       sync.Mutex
	nextID   int
	appended map[string]string
	messages chan gateway.QueueMessage
	fail     bool
}

func newMemoryOutboundQueue() *memoryOutboundQueue {
	return &memoryOutboundQueue{appended: make(map[string]string), messages: make(chan gateway.QueueMessage, 32)}
}

func (q *memoryOutboundQueue) Append(_ context.Context, deliveryID string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail {
		return "", errors.New("queue unavailable")
	}
	if id := q.appended[deliveryID]; id != "" {
		return id, nil
	}
	q.nextID++
	id := fmt.Sprintf("%d-0", q.nextID)
	q.appended[deliveryID] = id
	q.messages <- gateway.QueueMessage{ID: id, DeliveryID: deliveryID}
	return id, nil
}

func (q *memoryOutboundQueue) Read(ctx context.Context, _ string, _ int64) ([]gateway.QueueMessage, error) {
	select {
	case message := <-q.messages:
		return []gateway.QueueMessage{message}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *memoryOutboundQueue) Ack(context.Context, string) error { return nil }

func (q *memoryOutboundQueue) ClaimStale(context.Context, string, time.Duration, int64) ([]gateway.QueueMessage, error) {
	return nil, nil
}

func (q *memoryOutboundQueue) MoveToDLQAndAck(context.Context, gateway.QueueMessage, gateway.DLQMetadata) error {
	return nil
}

func (q *memoryOutboundQueue) setFail(fail bool) {
	q.mu.Lock()
	q.fail = fail
	q.mu.Unlock()
}

func TestE2E_GatewayOutbound(t *testing.T) {
	h := setupE2E(t)
	const token = "910001:outbound-secret-token"
	const configuredChatID int64 = -100910001
	h.fake.RegisterBot(token, "outbound_bot", 910001)
	h.fake.RegisterChat(token, configuredChatID, "Outbound group")

	accountID, err := h.store.AddBotAccount(models.BotAccount{Name: "Outbound Bot", Username: "outbound_bot", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	destinationID, err := h.store.AddTelegramDestination(models.TelegramDestination{
		Name: "Outbound Group", BotAccountID: accountID, ChatID: configuredChatID, ChatTitle: "Outbound group", Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	routeID, err := h.store.AddBusinessRoute(models.BusinessRoute{
		RouteKey: "alerts", DisplayName: "Alerts", BotAccountID: accountID, DestinationID: destinationID,
		OutboundEnabled: true, Enabled: true, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if routeID == 0 {
		t.Fatal("route was not created")
	}

	const credential = "gw_test_workload_secret"
	workloadID, err := h.store.CreateGatewayWorkload("it_monitor", "initial", auth.HashAPIKey(credential))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []models.GatewayAction{models.GatewayActionMessagesSend, models.GatewayActionCallbacksAnswer, models.GatewayActionDeliveriesRead} {
		if err := h.store.GrantGatewayPermission(workloadID, "alerts", action); err != nil {
			t.Fatal(err)
		}
	}
	const forbiddenCredential = "gw_forbidden_workload_secret"
	if _, err := h.store.CreateGatewayWorkload("untrusted", "initial", auth.HashAPIKey(forbiddenCredential)); err != nil {
		t.Fatal(err)
	}

	queue := newMemoryOutboundQueue()
	service := gateway.NewService(h.store, queue, gateway.NewTelegramHTTPClient(h.fake.URL()), "test-worker")
	service.Start(context.Background())
	t.Cleanup(service.Stop)
	h.server.SetGatewayService(service)
	h.ts = httptest.NewServer(h.server.BuildMux())
	t.Cleanup(h.ts.Close)

	call := func(method, path, bearer, idempotencyKey string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, marshalErr := json.Marshal(body)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			reader = bytes.NewReader(data)
		}
		req, requestErr := http.NewRequest(method, h.ts.URL+path, reader)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		resp, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer resp.Body.Close()
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, forbidden := range []string{token, strconv.FormatInt(configuredChatID, 10), credential, forbiddenCredential} {
			if bytes.Contains(data, []byte(forbidden)) {
				t.Fatalf("business response exposed a credential or physical destination: %s", data)
			}
		}
		return resp.StatusCode, data
	}

	status, body := call(http.MethodPost, "/api/v1/routes/alerts/messages", "", "missing-auth", map[string]any{"text": "hello"})
	if status != http.StatusUnauthorized || !bytes.Contains(body, []byte(`"error":"unauthorized"`)) {
		t.Fatalf("unauthenticated status=%d body=%s", status, body)
	}
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", forbiddenCredential, "forbidden", map[string]any{"text": "hello"})
	if status != http.StatusForbidden || !bytes.Contains(body, []byte(`"error":"forbidden"`)) {
		t.Fatalf("forbidden status=%d body=%s", status, body)
	}
	status, body = call(http.MethodPost, "/api/v1/routes/missing/messages", credential, "missing-route", map[string]any{"text": "hello"})
	if status != http.StatusNotFound || !bytes.Contains(body, []byte(`"error":"unknown_route"`)) {
		t.Fatalf("unknown route status=%d body=%s", status, body)
	}
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "", map[string]any{"text": "hello"})
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"error":"idempotency_required"`)) {
		t.Fatalf("missing idempotency status=%d body=%s", status, body)
	}
	for _, physicalOverride := range []map[string]any{
		{"text": "hello", "chat_id": -99}, {"text": "hello", "bot_id": 17}, {"text": "hello", "token": "caller-token"},
	} {
		status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "override-"+fmt.Sprint(len(physicalOverride)), physicalOverride)
		if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"error":"invalid_payload"`)) {
			t.Fatalf("physical override status=%d body=%s", status, body)
		}
	}

	payload := map[string]any{
		"text": "alert <b>now</b>", "parse_mode": "HTML",
		"reply_markup":     map[string]any{"inline_keyboard": []any{[]any{map[string]any{"text": "Approve", "callback_data": "approve:42"}}}},
		"reply_parameters": map[string]any{"message_id": 77, "allow_sending_without_reply": true},
	}
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "alert-42", payload)
	if status != http.StatusAccepted {
		t.Fatalf("accepted status=%d body=%s", status, body)
	}
	var accepted map[string]string
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatal(err)
	}
	deliveryID := accepted["delivery_id"]
	if deliveryID == "" || accepted["status"] != "accepted" {
		t.Fatalf("invalid acceptance: %s", body)
	}

	status, replayBody := call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "alert-42", payload)
	var replay map[string]string
	_ = json.Unmarshal(replayBody, &replay)
	if status != http.StatusAccepted || replay["delivery_id"] != deliveryID {
		t.Fatalf("idempotent replay status=%d body=%s", status, replayBody)
	}
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "alert-42", map[string]string{"text": "different"})
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"error":"idempotency_conflict"`)) {
		t.Fatalf("idempotency conflict status=%d body=%s", status, body)
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.fake.RequestsCountFor("sendMessage") != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.fake.RequestsCountFor("sendMessage") != 1 {
		stored, storedErr := h.store.GetGatewayDelivery(context.Background(), routeID, deliveryID)
		t.Fatalf("Telegram send not observed; delivery=%+v store_error=%v", stored, storedErr)
	}
	requests := h.fake.RequestsFor("sendMessage")
	var sent struct {
		ChatID          int64                   `json:"chat_id"`
		Text            string                  `json:"text"`
		ParseMode       string                  `json:"parse_mode"`
		ReplyMarkup     gateway.ReplyMarkup     `json:"reply_markup"`
		ReplyParameters gateway.ReplyParameters `json:"reply_parameters"`
	}
	if err := json.Unmarshal(requests[0].body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ChatID != configuredChatID || sent.Text != "alert <b>now</b>" || sent.ParseMode != "HTML" ||
		len(sent.ReplyMarkup.InlineKeyboard) != 1 || sent.ReplyMarkup.InlineKeyboard[0][0].CallbackData != "approve:42" ||
		sent.ReplyParameters.MessageID != 77 || !sent.ReplyParameters.AllowSendingWithoutReply {
		t.Fatalf("Telegram payload was not resolved/preserved: %+v", sent)
	}

	status, body = call(http.MethodGet, "/api/v1/routes/alerts/deliveries/"+deliveryID, credential, "", nil)
	if status != http.StatusOK {
		t.Fatalf("delivery query status=%d body=%s", status, body)
	}
	var delivery models.GatewayDelivery
	if err := json.Unmarshal(body, &delivery); err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "succeeded" || delivery.RouteKey != "alerts" || delivery.Action != models.GatewayActionMessagesSend || delivery.AttemptCount != 1 {
		t.Fatalf("unexpected delivery: %+v", delivery)
	}
	if bytes.Contains(body, []byte("alert <b>now</b>")) || bytes.Contains(body, []byte("approve:42")) {
		t.Fatal("delivery status exposed the outbound payload")
	}

	const callbackID = "callback-query-9001"
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/callbacks/"+callbackID+"/answer", credential, "callback-answer-1", map[string]any{
		"text": "Approved", "show_alert": true,
	})
	if status != http.StatusAccepted {
		t.Fatalf("callback acceptance status=%d body=%s", status, body)
	}
	var callbackAccepted map[string]string
	_ = json.Unmarshal(body, &callbackAccepted)
	h.Eventually(func() bool { return h.fake.RequestsCountFor("answerCallbackQuery") == 1 }, 2*time.Second, "callback answer")
	var callbackPayload map[string]any
	if err := json.Unmarshal(h.fake.RequestsFor("answerCallbackQuery")[0].body, &callbackPayload); err != nil {
		t.Fatal(err)
	}
	if callbackPayload["callback_query_id"] != callbackID || callbackPayload["text"] != "Approved" || callbackPayload["show_alert"] != true {
		t.Fatalf("callback payload mismatch: %+v", callbackPayload)
	}
	status, body = call(http.MethodGet, "/api/v1/routes/alerts/deliveries/"+callbackAccepted["delivery_id"], credential, "", nil)
	if status != http.StatusOK || bytes.Contains(body, []byte(callbackID)) || !bytes.Contains(body, []byte(`"status":"succeeded"`)) {
		t.Fatalf("callback delivery query status=%d body=%s", status, body)
	}

	queue.setFail(true)
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "queue-recovery", map[string]string{"text": "queue recovery"})
	if status != http.StatusServiceUnavailable || !bytes.Contains(body, []byte(`"error":"queue_unavailable"`)) {
		t.Fatalf("queue failure status=%d body=%s", status, body)
	}
	queue.setFail(false)
	status, body = call(http.MethodPost, "/api/v1/routes/alerts/messages", credential, "queue-recovery", map[string]string{"text": "queue recovery"})
	if status != http.StatusAccepted {
		t.Fatalf("queue recovery status=%d body=%s", status, body)
	}
}
