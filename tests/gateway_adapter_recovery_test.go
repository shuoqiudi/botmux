package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

type adapterJenkins struct {
	mu             sync.Mutex
	server         *httptest.Server
	mode           string
	triggerStarted chan struct{}
	triggers       int
	queueReads     int
	parameters     []map[string]string
	request        map[string]any
}

func newAdapterJenkins(t *testing.T, mode string) *adapterJenkins {
	j := &adapterJenkins{mode: mode, triggerStarted: make(chan struct{})}
	j.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		user, token, ok := r.BasicAuth()
		if !ok || user != "gateway" || token != "jenkins-secret" {
			t.Error("Jenkins credential missing")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/job/manage/buildWithParameters":
			j.triggers++
			_ = r.ParseForm()
			raw, _ := base64.StdEncoding.DecodeString(r.Form.Get("IT_MANAGE_REQUEST_JSON_B64"))
			_ = json.Unmarshal(raw, &j.request)
			j.parameters = []map[string]string{{"name": "IT_MANAGE_REQUEST_JSON_B64", "value": r.Form.Get("IT_MANAGE_REQUEST_JSON_B64")}, {"name": "IT_MANAGE_REQUEST_LABEL", "value": r.Form.Get("IT_MANAGE_REQUEST_LABEL")}}
			if j.mode == "slow_trigger" {
				close(j.triggerStarted)
				time.Sleep(250 * time.Millisecond)
			}
			if j.mode == "rejected" {
				w.WriteHeader(403)
				io.WriteString(w, "password=jenkins-secret")
				return
			}
			if j.mode == "ambiguous" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Location", j.server.URL+"/queue/item/17/")
			w.WriteHeader(201)
		case "/queue/item/17/api/json":
			j.queueReads++
			if j.mode == "network" || (j.mode == "transient" && j.queueReads == 1) {
				w.WriteHeader(503)
				return
			}
			if j.mode == "queued" {
				io.WriteString(w, `{"cancelled":false}`)
				return
			}
			if j.mode == "queue_missing" || j.mode == "queue_missing_network" {
				w.WriteHeader(404)
				return
			}
			fmt.Fprintf(w, `{"executable":{"number":23,"url":%q}}`, j.server.URL+"/job/manage/23/")
		case "/queue/api/json":
			if j.mode == "queue_missing_network" {
				w.WriteHeader(503)
				return
			}
			if j.mode == "queue_missing" {
				io.WriteString(w, `{"items":[]}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": 17, "task": map[string]any{"url": j.server.URL + "/job/manage/"}, "actions": []any{map[string]any{"parameters": j.parameters}}}}})
		case "/job/manage/api/json":
			if r.URL.Query().Get("tree") == "buildable" {
				io.WriteString(w, `{"buildable":true}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": []any{map[string]any{"number": 23, "queueId": 17, "actions": []any{map[string]any{"parameters": j.parameters}}}}})
		case "/job/manage/23/api/json":
			if j.mode == "building" {
				io.WriteString(w, `{"building":true,"result":null}`)
				return
			}
			if j.mode == "build_failed" {
				io.WriteString(w, `{"building":false,"result":"FAILURE"}`)
				return
			}
			io.WriteString(w, `{"building":false,"result":"SUCCESS"}`)
		case "/job/manage/23/consoleText":
			if j.mode == "missing_result" || j.mode == "build_failed" {
				io.WriteString(w, "raw console password=jenkins-secret")
				return
			}
			identity := j.request["identity"].(map[string]any)
			status, code := "succeeded", ""
			if j.mode == "business_failed" {
				status, code = "business_failed", "BUSINESS_FAILED"
			}
			id := identity["request_id"]
			if j.mode == "wrong_identity" {
				id = "some-other-request"
			}
			raw, _ := json.Marshal(map[string]any{"schema": "it_manage.management-result/v1", "request_id": id, "delivery_id": identity["delivery_id"], "status": status, "code": code, "detail": "Authorization: Basic secret; password=jenkins-secret", "execution": map[string]any{"execution_id": "malicious secret"}})
			fmt.Fprintf(w, "IT_MANAGE_MANAGEMENT_RESULT_BEGIN:%s:IT_MANAGE_MANAGEMENT_RESULT_END\n", base64.StdEncoding.EncodeToString(raw))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(j.server.Close)
	return j
}
func (j *adapterJenkins) setMode(mode string) { j.mu.Lock(); j.mode = mode; j.mu.Unlock() }
func (j *adapterJenkins) triggerCount() int   { j.mu.Lock(); defer j.mu.Unlock(); return j.triggers }

type adapterHarness struct {
	t        *testing.T
	path     string
	store    *store.Store
	fake     *fakeTG
	jenkins  *adapterJenkins
	redis    *redis.Client
	queue    *gateway.RedisInboundQueue
	outbound *gateway.Service
	inbound  *gateway.Inbound
	config   gateway.AdapterConfig
	cancel   context.CancelFunc
	done     chan error
	botID    int64
}

func newAdapterHarness(t *testing.T, mode string) *adapterHarness {
	h := &adapterHarness{t: t, path: filepath.Join(t.TempDir(), "adapter.db"), fake: newFakeTG(t), jenkins: newAdapterJenkins(t, mode)}
	h.redis = redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() { h.redis.Close() })
	if err := h.redis.Ping(context.Background()).Err(); err != nil {
		t.Skip("requires Redis at 127.0.0.1:6379")
	}
	h.queue = gateway.NewRedisInboundQueueWithNamespace(h.redis, "adapter:"+uuid.NewString())
	var err error
	h.store, err = store.NewStore(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.fake.RegisterBot("1234:tg-secret", "adapter_bot", 1234)
	h.botID, err = h.store.RegisterCLIBot("1234:tg-secret", "adapter_bot")
	if err != nil {
		t.Fatal(err)
	}
	account, err := h.store.AddBotAccount(models.BotAccount{Name: "Adapter", Username: "adapter_bot", Token: "1234:tg-secret"})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := h.store.AddTelegramDestination(models.TelegramDestination{Name: "Original chat", BotAccountID: account, ChatID: -1234, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.store.AddBusinessRoute(models.BusinessRoute{RouteKey: "it_manage", DisplayName: "Management", BotAccountID: account, DestinationID: dest, InboundTarget: "it_manage", InboundEnabled: true, OutboundEnabled: true, Enabled: true, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	h.config = gateway.AdapterConfig{JobURL: h.jenkins.server.URL + "/job/manage", Username: "gateway", APIToken: "jenkins-secret", PollInterval: 15 * time.Millisecond, QueueTimeout: 2 * time.Second, BuildTimeout: 2 * time.Second, RequestTimeout: 100 * time.Millisecond, ReplyTimeout: 2 * time.Second, MaxFailures: 2}
	h.start()
	t.Cleanup(func() { h.stop(); h.store.Close() })
	return h
}
func (h *adapterHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.outbound = gateway.NewService(h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(h.fake.URL()), "")
	h.outbound.Start(ctx)
	adapter, err := gateway.NewAdapter(h.store, h.outbound, h.config, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.inbound = gateway.NewInbound(h.store, h.queue, nil, gateway.InboundConfig{PollInterval: 10 * time.Millisecond, ClaimMinIdle: 10 * time.Millisecond})
	h.inbound.SetAdapter(adapter)
	h.done = make(chan error, 1)
	go func() { h.done <- h.inbound.Run(ctx) }()
}
func (h *adapterHarness) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.outbound.Stop()
		h.cancel = nil
	}
}
func (h *adapterHarness) ingest(text string, updateID int) string {
	result, err := h.inbound.IngestUpdate(context.Background(), h.botID, map[string]any{"update_id": updateID, "message": map[string]any{"message_id": 8, "message_thread_id": 9, "chat": map[string]any{"id": -1234}, "text": text}})
	if err != nil {
		h.t.Fatal(err)
	}
	return result.DeliveryID
}
func (h *adapterHarness) waitStage(id, stage string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := h.store.GetAdapterExecution(context.Background(), id)
		if state != nil && state.Stage == stage {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	state, err := h.store.GetAdapterExecution(context.Background(), id)
	h.t.Fatalf("want stage %s, got %+v %v", stage, state, err)
}
func (h *adapterHarness) finish(id string) *models.InboundDelivery {
	h.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := h.store.GetInboundDelivery(context.Background(), id)
		if d != nil && (d.Status == models.InboundSucceeded || d.Status == models.InboundDLQ) {
			return d
		}
		time.Sleep(15 * time.Millisecond)
	}
	state, err := h.store.GetAdapterExecution(context.Background(), id)
	h.t.Fatalf("delivery unfinished: %+v %v", state, err)
	return nil
}

func TestGatewayAdapterRestartAndRouteMigration(t *testing.T) {
	h := newAdapterHarness(t, "queued")
	id := h.ingest(`/it_manage@adapter_bot dns list`, 1234)
	h.waitStage(id, "queued")
	h.stop()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	h.store, err = store.NewStore(h.path)
	if err != nil {
		t.Fatal(err)
	}
	route, err := h.store.GetBusinessRoute("it_manage")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := h.store.GetTelegramDestination(route.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	destination.ChatID = -9999
	if err := h.store.MigrateTelegramDestination(*destination, destination.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	h.jenkins.setMode("normal")
	h.start()
	if got := h.ingest(`/it_manage@adapter_bot dns list`, 1234); got != id {
		t.Fatal("restarted Update lost identity")
	}
	if d := h.finish(id); d.Status != models.InboundSucceeded {
		t.Fatalf("restart failed: %+v", d)
	}
	if h.jenkins.triggerCount() != 1 {
		t.Fatal("restart triggered duplicate Jenkins work")
	}
	sent := h.fake.RequestsFor("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("duplicate replies: %d", len(sent))
	}
	for _, item := range sent {
		var payload map[string]any
		_ = json.Unmarshal(item.body, &payload)
		if payload["chat_id"] != float64(-1234) || payload["message_thread_id"] != float64(9) {
			t.Fatal("reply escaped original conversation/topic")
		}
	}
	detail, err := h.store.GetGatewayDeliveryDetail(context.Background(), "it_manage", id)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(detail)
	if !bytes.Contains(data, []byte(`"build_number":23`)) {
		t.Fatalf("operations lacks safe Jenkins correlation: %s", data)
	}
	for _, secret := range []string{"tg-secret", "jenkins-secret", "dns", "-1234", "console"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("operations leaked %s", secret)
		}
	}
}

func TestGatewayAdapterBoundedOutcomes(t *testing.T) {
	for _, tc := range []struct{ mode, code string }{
		{"rejected", "jenkins_trigger_failed"}, {"queued", "jenkins_queue_timeout"},
		{"building", "jenkins_build_timeout"}, {"build_failed", "jenkins_build_failed"},
		{"missing_result", "result_unavailable"}, {"wrong_identity", "result_unavailable"},
		{"business_failed", "business_failed"}, {"network", "jenkins_unavailable"},
		{"transient", ""}, {"ambiguous", ""}, {"queue_missing", ""}, {"queue_missing_network", "jenkins_unavailable"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			h := newAdapterHarness(t, tc.mode)
			id := h.ingest("/it_manage dns list", 2345)
			d := h.finish(id)
			if d.LastErrorClass != tc.code {
				t.Fatalf("error=%q want %q", d.LastErrorClass, tc.code)
			}
			if tc.code == "" && d.Status != models.InboundSucceeded || tc.code != "" && d.Status != models.InboundDLQ {
				t.Fatalf("incorrect terminal status: %+v", d)
			}
			if h.jenkins.triggerCount() != 1 {
				t.Fatal("failure recovery resubmitted Jenkins")
			}
			sent := h.fake.RequestsFor("sendMessage")
			if len(sent) != 2 {
				t.Fatalf("missing acknowledgment/result: %d", len(sent))
			}
			for _, item := range sent {
				if bytes.Contains(item.body, []byte("jenkins-secret")) || bytes.Contains(item.body, []byte("malicious")) {
					t.Fatal("untrusted result leaked")
				}
			}
		})
	}
}

func TestGatewayAdapterIgnoresUnrelatedMessages(t *testing.T) {
	h := newAdapterHarness(t, "normal")
	for n, text := range []string{"hello", "/it_manage_extra dns list", "/it_manage@another_bot dns list", "prefix /it_manage dns list"} {
		id := h.ingest(text, 3456+n)
		if d := h.finish(id); d.Status != models.InboundSucceeded {
			t.Fatalf("unrelated message not safely consumed: %+v", d)
		}
	}
	if h.jenkins.triggerCount() != 0 || h.fake.RequestsCountFor("sendMessage") != 0 {
		t.Fatal("unrelated text caused side effects")
	}
	id := h.ingest(`/it_manage dns "unclosed`, 4000)
	if d := h.finish(id); d.LastErrorClass != "invalid_request" {
		t.Fatalf("invalid generic arguments were not rejected: %+v", d)
	}
	if h.jenkins.triggerCount() != 0 {
		t.Fatal("invalid request reached Jenkins")
	}
}

func TestGatewayAdapterAmbiguousTriggerRestart(t *testing.T) {
	h := newAdapterHarness(t, "ambiguous")
	id := h.ingest("/it_manage dns list", 5000)
	h.waitStage(id, "triggering")
	h.stop()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	h.store, err = store.NewStore(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.start()
	if d := h.finish(id); d.Status != models.InboundSucceeded {
		t.Fatalf("ambiguous POST did not reconcile: %+v", d)
	}
	if h.jenkins.triggerCount() != 1 {
		t.Fatal("ambiguous POST was repeated after restart")
	}
}

func TestGatewayAdapterHealthAndSecretConfig(t *testing.T) {
	h := newAdapterHarness(t, "normal")
	path := filepath.Join(t.TempDir(), "adapter.json")
	raw := fmt.Sprintf(`{"job_url":%q,"username":"gateway","api_token":"jenkins-secret","poll_interval_seconds":1}`, h.jenkins.server.URL+"/job/manage")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := gateway.LoadAdapterConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(config)
	if bytes.Contains(encoded, []byte("jenkins-secret")) {
		t.Fatal("config exposed Jenkins secret")
	}
	adapter, err := gateway.NewAdapter(h.store, h.outbound, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	ops := gateway.NewOperations(h.store, nil, nil, h.outbound, h.inbound, gateway.NewTelegramHTTPClient(h.fake.URL()), nil)
	ops.SetAdapter(adapter)
	health := ops.Health(context.Background())
	if health.Adapter.Status != "healthy" || health.Jenkins.Status != "healthy" || health.Redis.Status != "unavailable" {
		t.Fatalf("component links conflated: %+v", health)
	}
	if err := os.WriteFile(path, []byte(`{"job_url":"invalid","api_token":"secret-in-invalid-config"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = gateway.LoadAdapterConfig(path)
	if err == nil || strings.Contains(err.Error(), "secret-in-invalid-config") {
		t.Fatal("invalid secret config did not fail safely")
	}
}

func TestGatewayAdapterConcurrentReclaim(t *testing.T) {
	h := newAdapterHarness(t, "queued")
	adapter, err := gateway.NewAdapter(h.store, h.outbound, h.config, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := gateway.NewInbound(h.store, h.queue, nil, gateway.InboundConfig{PollInterval: 10 * time.Millisecond, ClaimMinIdle: 10 * time.Millisecond})
	second.SetAdapter(adapter)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- second.Run(ctx) }()
	defer func() { cancel(); <-done }()
	id := h.ingest("/it_manage arbitrary action", 6000)
	h.waitStage(id, "queued")
	for n := 0; n < 5; n++ {
		if h.ingest("/it_manage arbitrary action", 6000) != id {
			t.Fatal("duplicate Update changed Delivery")
		}
	}
	h.jenkins.setMode("normal")
	if d := h.finish(id); d.Status != models.InboundSucceeded {
		t.Fatalf("concurrent reclaim failed: %+v", d)
	}
	if h.jenkins.triggerCount() != 1 {
		t.Fatal("concurrent reclaim created duplicate Jenkins execution")
	}
	if h.fake.RequestsCountFor("sendMessage") != 2 {
		t.Fatal("concurrent reclaim duplicated replies")
	}
}

func TestGatewayAdapterInterruptedDispatch(t *testing.T) {
	h := newAdapterHarness(t, "slow_trigger")
	id := h.ingest("/it_manage dns list", 7000)
	select {
	case <-h.jenkins.triggerStarted:
	case <-time.After(4 * time.Second):
		t.Fatal("trigger did not arrive")
	}
	// Cancel while the POST is in flight. The deferred state save uses the
	// cancelled context, leaving the durable intent and lease for reclaim.
	h.stop()
	state, err := h.store.GetAdapterExecution(context.Background(), id)
	if err != nil || state.Stage != "triggering" || state.QueueID != 0 {
		t.Fatalf("dispatch intent not retained: %+v %v", state, err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.store, err = store.NewStore(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.start()
	if d := h.finish(id); d.Status != models.InboundSucceeded {
		t.Fatalf("interrupted dispatch did not recover: %+v", d)
	}
	if h.jenkins.triggerCount() != 1 {
		t.Fatal("interrupted dispatch was submitted twice")
	}
}
