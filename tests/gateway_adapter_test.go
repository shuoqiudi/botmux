package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
)

func TestGatewayAdapterRouteAPI(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	const token = "912340:adapter-test-secret"
	h.fake.RegisterBot(token, "adapter_bot", 912340)
	h.fake.RegisterChat(token, -912340, "Adapter test")
	payload := map[string]any{
		"bot_account": map[string]any{"name": "Adapter", "token": token},
		"destination": map[string]any{"name": "Adapter", "chat_id": -912340},
		"route": map[string]any{"route_key": "it_manage", "display_name": "Management", "inbound_enabled": true,
			"inbound_target": "it_manage", "outbound_enabled": true, "enabled": true},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/api/gateway/v1/routes/setup", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("embedded route rejected: %d %s", resp.StatusCode, data)
	}
	var route map[string]any
	if err := json.Unmarshal(data, &route); err != nil {
		t.Fatal(err)
	}
	if route["inbound_target"] != "it_manage" || route["backend_credential_set"] != false {
		t.Fatalf("incorrect route target: %s", data)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("credential exposed")
	}
}

// One serial flow crosses SQLite, Redis Streams, the embedded adapter and both
// HTTP protocol boundaries. Jenkins receives the literal v1 contract.
func TestGatewayAdapterRoundTrip(t *testing.T) {
	h := setupE2E(t)
	const token = "912341:telegram-secret"
	const chatID int64 = -912341
	botID := h.AddBot(models.BotConfig{Name: "Adapter", Token: token})
	account, err := h.store.AddBotAccount(models.BotAccount{Name: "Adapter", Username: "adapter_bot", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := h.store.AddTelegramDestination(models.TelegramDestination{Name: "Adapter", BotAccountID: account, ChatID: chatID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.store.AddBusinessRoute(models.BusinessRoute{RouteKey: "it_manage", DisplayName: "Management", BotAccountID: account, DestinationID: dest, InboundTarget: "it_manage", InboundEnabled: true, OutboundEnabled: true, Enabled: true, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var request map[string]any
	var jenkins *httptest.Server
	jenkins = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "gateway" || secret != "jenkins-secret" {
			t.Error("missing Jenkins secret authentication")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/job/manage/buildWithParameters":
			requests.Add(1)
			_ = r.ParseForm()
			raw, err := base64.StdEncoding.DecodeString(r.Form.Get("IT_MANAGE_REQUEST_JSON_B64"))
			if err != nil || json.Unmarshal(raw, &request) != nil {
				t.Error("invalid Management Request")
			}
			if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte(strconv.FormatInt(chatID, 10))) {
				t.Error("Telegram routing data leaked to Jenkins")
			}
			w.Header().Set("Location", jenkins.URL+"/queue/item/17/")
			w.WriteHeader(201)
		case "/queue/item/17/api/json":
			fmt.Fprintf(w, `{"executable":{"number":23,"url":%q}}`, jenkins.URL+"/job/manage/23/")
		case "/job/manage/23/api/json":
			io.WriteString(w, `{"building":false,"result":"SUCCESS"}`)
		case "/job/manage/23/consoleText":
			identity := request["identity"].(map[string]any)
			raw, _ := json.Marshal(map[string]any{"schema": "it_manage.management-result/v1", "request_id": identity["request_id"], "delivery_id": identity["delivery_id"], "status": "succeeded", "code": "", "detail": "password=jenkins-secret"})
			fmt.Fprintf(w, "untrusted console secret\nIT_MANAGE_MANAGEMENT_RESULT_BEGIN:%s:IT_MANAGE_MANAGEMENT_RESULT_END\n", base64.StdEncoding.EncodeToString(raw))
		default:
			w.WriteHeader(404)
		}
	}))
	defer jenkins.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Skip("requires test Redis at 127.0.0.1:6379")
	}
	q := gateway.NewRedisInboundQueueWithNamespace(redisClient, "adapter-test:"+uuid.NewString())
	service := gateway.NewService(h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(h.fake.URL()), "")
	service.Start(ctx)
	defer service.Stop()
	adapter, err := gateway.NewAdapter(h.store, service, gateway.AdapterConfig{JobURL: jenkins.URL + "/job/manage", Username: "gateway", APIToken: "jenkins-secret", PollInterval: 10 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inbound := gateway.NewInbound(h.store, q, nil, gateway.InboundConfig{PollInterval: 10 * time.Millisecond, ClaimMinIdle: 10 * time.Millisecond})
	inbound.SetAdapter(adapter)
	done := make(chan error, 1)
	go func() { done <- inbound.Run(ctx) }()
	defer func() { cancel(); <-done }()
	update := map[string]any{"update_id": 1234, "message": map[string]any{"message_id": 8, "chat": map[string]any{"id": chatID}, "text": `/it_manage future-module future-action "two words" --flag='a b'`}}
	result, err := inbound.IngestUpdate(ctx, botID, update)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := inbound.IngestUpdate(ctx, botID, update)
	if err != nil || !duplicate.Duplicate || duplicate.DeliveryID != result.DeliveryID {
		t.Fatal("duplicate changed identity")
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := h.store.GetInboundDelivery(ctx, result.DeliveryID)
		if d != nil && d.Status == models.InboundSucceeded {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	d, err := h.store.GetInboundDelivery(ctx, result.DeliveryID)
	if err != nil || d.Status != models.InboundSucceeded {
		t.Fatalf("delivery did not complete: %+v %v", d, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("Jenkins executions=%d", requests.Load())
	}
	if request["schema"] != "it_manage.management-request/v1" || !reflect.DeepEqual(request["argv"], []any{"future-module", "future-action", "two words", "--flag=a b"}) {
		t.Fatalf("contract mismatch: %v", request)
	}
	sent := h.fake.RequestsFor("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("want acknowledgment and final result, got %d", len(sent))
	}
	for _, item := range sent {
		var msg map[string]any
		_ = json.Unmarshal(item.body, &msg)
		if msg["chat_id"] != float64(chatID) || bytes.Contains(item.body, []byte("jenkins-secret")) {
			t.Fatal("unsafe reply")
		}
	}
	if !bytes.Contains(sent[0].body, []byte("accepted")) || !bytes.Contains(sent[1].body, []byte("succeeded")) {
		t.Fatal("reply order or result incorrect")
	}
}
