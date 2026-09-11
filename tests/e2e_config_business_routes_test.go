package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/configbackup"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

func TestE2E_ConfigBusinessRouteScriptRoundTrip(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	target := setupE2E(t, withHTTPServer())
	startConfigGateway(t, target)
	const token = "950001:route-bot-secret"
	const chatID int64 = -100950001
	account, err := source.store.AddBotAccount(models.BotAccount{Name: "Routes", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := source.store.AddTelegramDestination(models.TelegramDestination{Name: "Operations", BotAccountID: account, ChatID: chatID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.store.AddBusinessRoute(models.BusinessRoute{RouteKey: "operations", DisplayName: "Operations", BotAccountID: account, DestinationID: destination, OutboundEnabled: true, Enabled: true, Status: "active", AllowedCallers: []string{"producer"}, LastValidatedAt: "2020-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := source.store.CreateGatewayWorkload("producer", "initial", auth.HashAPIKey("original-route-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if err = source.store.GrantGatewayPermission(workload, "operations", models.GatewayActionMessagesSend); err != nil {
		t.Fatal(err)
	}
	target.fake.RegisterBot(token, "routes", 950001)
	target.fake.RegisterChat(token, chatID, "Operations")
	file := filepath.Join(t.TempDir(), "routes.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out, err := configScript(t, target, "restore", file); err != nil {
			t.Fatalf("restore: %v %s", err, out)
		}
	}
	status, body := serviceCall(t, target, "POST", "/api/v1/routes/operations/messages", "original-route-credential", "new", map[string]any{"text": "restored business message"}, false)
	if status != 202 {
		t.Fatalf("send: %d %s", status, body)
	}
	target.Eventually(func() bool { return len(target.fake.RequestsFor("sendMessage")) == 1 }, 5*time.Second, "restored route sends")
	request := target.fake.RequestsFor("sendMessage")[0]
	var sent struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(request.body, &sent); err != nil {
		t.Fatal(err)
	}
	if request.token != token || sent.ChatID != chatID || sent.Text != "restored business message" {
		t.Fatal("wrong restored recipient or message")
	}
	status, body = serviceCall(t, target, "GET", "/api/gateway/v1/routes", "", "", nil, true)
	if status != 200 || bytes.Contains(body, []byte("2020-01-01")) {
		t.Fatalf("old validation restored: %d %s", status, body)
	}
	again := filepath.Join(t.TempDir(), "again.json")
	if out, err := configScript(t, target, "export", again); err != nil {
		t.Fatalf("reexport: %v %s", err, out)
	}
	restored, err := os.ReadFile(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, restored) {
		t.Fatal("route configuration changed across instances")
	}
}

func TestE2E_ConfigBusinessRouteBidirectionalAndSharedSource(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	botConfig, err := f.h.store.GetBotConfig(f.account.NativeBotID)
	if err != nil {
		t.Fatal(err)
	}
	botConfig.ManageEnabled = true
	botConfig.PollingTimeout = 1
	if err = f.h.store.UpdateBotConfig(*botConfig); err != nil {
		t.Fatal(err)
	}
	const token = "940001:subscription-secret"
	received := make(chan backendReceipt, 16)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var update map[string]any
		if r.URL.Path != "/updates" || json.NewDecoder(r.Body).Decode(&update) != nil {
			t.Error("wrong backend URL or payload")
			w.WriteHeader(400)
			return
		}
		received <- backendReceipt{Update: update, Headers: r.Header.Clone()}
		w.WriteHeader(202)
	}))
	defer backend.Close()
	// Shared notification source: adding route grants must not widen service or
	// other route permissions, or duplicate the existing subscription.
	workloads, err := f.h.store.GetGatewayWorkloadsAdmin()
	if err != nil {
		t.Fatal(err)
	}
	var workload int64
	for _, w := range workloads {
		if w.Name == "Subscriptions" {
			workload = w.ID
		}
	}
	if workload == 0 {
		t.Fatalf("missing subscriber: %+v", workloads)
	}
	for i, key := range []string{"duplex", "no-inbound", "no-outbound", "disabled", "embedded"} {
		destination := f.destination.ID
		if i > 0 {
			destination, err = f.h.store.AddTelegramDestination(models.TelegramDestination{Name: key, BotAccountID: f.account.ID, ChatID: -100940001 - int64(i), Status: "active"})
			if err != nil {
				t.Fatal(err)
			}
		}
		route := models.BusinessRoute{RouteKey: key, DisplayName: key, BotAccountID: f.account.ID, DestinationID: destination, InboundTarget: "backend", InboundEnabled: key != "no-inbound", OutboundEnabled: key != "no-outbound", Enabled: key != "disabled", Status: "active", InboundBackendURL: backend.URL + "/updates", InboundBackendHealthURL: backend.URL + "/health", InboundBackendToken: "backend-original-secret", AllowedCallers: []string{"subscriber"}}
		if key == "disabled" {
			route.Status = "disabled"
		}
		if key == "embedded" {
			route.InboundTarget = "it_manage"
			route.InboundEnabled = false
			route.InboundBackendURL = ""
			route.InboundBackendHealthURL = ""
			route.InboundBackendToken = ""
		}
		if _, err = f.h.store.AddBusinessRoute(route); err != nil {
			t.Fatal(err)
		}
		if err = f.h.store.GrantGatewayPermission(workload, key, models.GatewayActionMessagesSend); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.h.store.CreateGatewayWorkload("untrusted", "initial", auth.HashAPIKey("untrusted-original")); err != nil {
		t.Fatal(err)
	}
	target := setupE2E(t, withHTTPServer())
	target.fake.RegisterBot(token, "subscriber", 940001)
	for i := 0; i < 5; i++ {
		target.fake.RegisterChat(token, -100940001-int64(i), "Target")
	}
	// Force local route, source, destination and Bot IDs to differ.
	if _, err = target.store.DB().Exec(`INSERT INTO sqlite_sequence(name,seq) VALUES('gateway_business_routes',100),('bots',100),('gateway_telegram_destinations',100); UPDATE sqlite_sequence SET seq=100 WHERE name='gateway_workloads'`); err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = client.Ping(ctx).Err(); err != nil {
		t.Skip("requires test Redis at 127.0.0.1:6379")
	}
	queue := gateway.NewRedisInboundQueueWithNamespace(client, "config-routes:"+uuid.NewString())
	inbound := gateway.NewInbound(target.store, queue, nil, gateway.InboundConfig{PollInterval: 5 * time.Millisecond, ClaimMinIdle: 10 * time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- inbound.Run(ctx) }()
	defer func() { target.proxy.StopAll(); cancel(); <-done }()
	worker := gateway.NewService(target.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(target.fake.URL()), "restored")
	target.server.SetGatewayService(worker)
	worker.Start(ctx)
	defer worker.Stop()
	file := filepath.Join(t.TempDir(), "duplex.json")
	if out, err := configScript(t, f.h, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("backend-original-secret")) {
		t.Fatal("backend secret missing")
	}
	if out, err := configScript(t, target, "restore", file); err == nil || !bytes.Contains(out, []byte("gateway_inbound")) {
		t.Fatalf("missing inbound worker reported loaded: %v %s", err, out)
	}
	target.proxy.SetInboundGateway(inbound)
	target.Eventually(func() bool { running, _ := inbound.WorkerHealth(); return running }, time.Second, "inbound worker started")
	for i := 0; i < 2; i++ {
		out, err := configScript(t, target, "restore", file)
		if err != nil || !bytes.Contains(out, []byte("runtime loaded: True; external health: not verified")) {
			t.Fatalf("restore receipt: %v %s", err, out)
		}
	}
	for _, tc := range []struct {
		route, credential string
		want              int
	}{{"duplex", "subscriber-secret", 202}, {"no-outbound", "subscriber-secret", 409}, {"disabled", "subscriber-secret", 409}, {"duplex", "untrusted-original", 403}} {
		status, body := serviceCall(t, target, "POST", "/api/v1/routes/"+tc.route+"/messages", tc.credential, tc.route, map[string]any{"text": "business"}, false)
		if status != tc.want {
			t.Fatalf("%s: %d want %d %s", tc.route, status, tc.want, body)
		}
	}
	status, _ := serviceCall(t, target, "POST", "/api/v1/routes/duplex/callbacks/unknown/answer", "subscriber-secret", "callback", map[string]any{"text": "answer"}, false)
	if status != 403 {
		t.Fatalf("callback grant widened: %d", status)
	}
	status, body := serviceCall(t, target, "POST", "/api/v1/services/notifications", "subscriber-secret", "shared", map[string]any{"fingerprint": "dns", "text": "notification"}, false)
	if status != 202 {
		t.Fatalf("shared source: %d %s", status, body)
	}
	var notification models.ServiceNotification
	if err = json.Unmarshal(body, &notification); err != nil {
		t.Fatal(err)
	}
	if notification.SubscriptionCount != 1 {
		t.Fatalf("shared subscription duplicated: %+v", notification)
	}
	// These updates travel through fake Telegram getUpdates and the actual poller.
	for i := 0; i < 4; i++ {
		target.fake.EnqueueUpdate(token, map[string]any{"update_id": 500 + i, "message": map[string]any{"message_id": 10 + i, "chat": map[string]any{"id": -100940001 - i, "type": "supergroup"}, "text": "incoming"}})
	}
	for i := 0; i < 2; i++ {
		select {
		case got := <-received:
			id := got.Update["update_id"].(float64)
			if id != 500 && id != 502 {
				t.Fatalf("disabled inbound delivered: %v", id)
			}
			if got.Headers.Get("Authorization") != "Bearer backend-original-secret" {
				t.Fatal("backend authentication changed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("restored inbound did not reach backend")
		}
	}
	target.Eventually(func() bool { return len(target.fake.RequestsFor("sendMessage")) == 2 }, 5*time.Second, "business and shared notification")
	for _, r := range target.fake.RequestsFor("sendMessage") {
		var sent struct {
			ChatID int64 `json:"chat_id"`
		}
		if json.Unmarshal(r.body, &sent) != nil || r.token != token || sent.ChatID != -100940001 {
			t.Fatal("restored Bot/chat mismatch")
		}
	}
	// Once the poller has durably consumed the last update, disabled updates must
	// have no backend delivery. Repeating restore must not duplicate active routes.
	target.Eventually(func() bool {
		bots, e := target.store.GetBotConfigs()
		return e == nil && len(bots) == 1 && bots[0].Offset >= 504
	}, 5*time.Second, "all updates consumed")
	select {
	case extra := <-received:
		t.Fatalf("unexpected backend delivery: %+v", extra.Update)
	case <-time.After(100 * time.Millisecond):
	}
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry after traffic: %v %s", err, out)
	}
}

func TestE2E_ConfigBusinessRouteValidationAndRollback(t *testing.T) {
	snapshot := configbackup.Empty()
	snapshot.Bots = []configbackup.Bot{{Ref: "bot", Name: "Bot", Token: "950003:secret"}}
	snapshot.Destinations = []configbackup.Destination{{Ref: "chat", BotRef: "bot", Name: "Chat", ChatID: "-100950003", Status: "active"}}
	snapshot.BusinessRoutes = []configbackup.BusinessRoute{{RouteKey: "operations", DisplayName: "Operations", BotRef: "bot", DestinationRef: "chat", InboundTarget: "backend", InboundEnabled: true, InboundBackendURL: "https://backend.example/updates", InboundBackendHealthURL: "https://backend.example/health", InboundBackendToken: "private-backend-secret", OutboundEnabled: true, Enabled: true, Status: "active", AllowedCallers: []string{"producer"}}}
	snapshot.Workloads = []configbackup.Workload{{Ref: "source", Name: "producer", Status: "active", Credentials: []configbackup.Credential{{Name: "initial", Algorithm: "sha256", Verifier: auth.HashAPIKey("source-secret"), Enabled: true}}, RoutePermissions: []configbackup.RoutePermission{{RouteKey: "operations", Action: "messages.send"}, {RouteKey: "operations", Action: "callbacks.answer"}, {RouteKey: "operations", Action: "deliveries.read"}}}}
	original, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	target := setupE2E(t, withHTTPServer())
	for _, tc := range []struct {
		name   string
		change func(*configbackup.Snapshot)
	}{
		{"missing bot", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].BotRef = "absent" }},
		{"missing destination", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].DestinationRef = "absent" }},
		{"wrong destination bot", func(s *configbackup.Snapshot) {
			s.Bots = append(s.Bots, configbackup.Bot{Ref: "other", Token: "950004:other"})
			s.Destinations[0].BotRef = "other"
		}},
		{"invalid route key", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].RouteKey = "Operations/secret" }},
		{"duplicate key", func(s *configbackup.Snapshot) { s.BusinessRoutes = append(s.BusinessRoutes, s.BusinessRoutes[0]) }},
		{"duplicate inbound", func(s *configbackup.Snapshot) {
			r := s.BusinessRoutes[0]
			r.RouteKey = "second"
			s.BusinessRoutes = append(s.BusinessRoutes, r)
		}},
		{"alias inbound", func(s *configbackup.Snapshot) {
			d := s.Destinations[0]
			d.Ref = "alias"
			s.Destinations = append(s.Destinations, d)
			r := s.BusinessRoutes[0]
			r.RouteKey = "second"
			r.DestinationRef = "alias"
			s.BusinessRoutes = append(s.BusinessRoutes, r)
		}},
		{"token alias inbound", func(s *configbackup.Snapshot) {
			b := s.Bots[0]
			b.Ref = "aliasbot"
			b.Token = "0950003:rotated-secret"
			s.Bots = append(s.Bots, b)
			d := s.Destinations[0]
			d.Ref = "alias"
			d.BotRef = "aliasbot"
			s.Destinations = append(s.Destinations, d)
			r := s.BusinessRoutes[0]
			r.RouteKey = "second"
			r.BotRef = "aliasbot"
			r.DestinationRef = "alias"
			s.BusinessRoutes = append(s.BusinessRoutes, r)
		}},
		{"empty name", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].DisplayName = " " }},
		{"invalid status", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].Status = "healthy" }},
		{"invalid target", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].InboundTarget = "shell" }},
		{"invalid backend", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].InboundBackendURL = "file:///secret" }},
		{"invalid health", func(s *configbackup.Snapshot) {
			s.BusinessRoutes[0].InboundBackendHealthURL = "https://user:secret@backend/health"
		}},
		{"adapter backend", func(s *configbackup.Snapshot) { s.BusinessRoutes[0].InboundTarget = "it_manage" }},
		{"adapter requires outbound", func(s *configbackup.Snapshot) {
			r := &s.BusinessRoutes[0]
			r.InboundTarget = "it_manage"
			r.InboundBackendURL = ""
			r.InboundBackendHealthURL = ""
			r.InboundBackendToken = ""
			r.OutboundEnabled = false
		}},
		{"missing permission route", func(s *configbackup.Snapshot) { s.Workloads[0].RoutePermissions[0].RouteKey = "absent" }},
		{"unknown action", func(s *configbackup.Snapshot) { s.Workloads[0].RoutePermissions[0].Action = "admin" }},
		{"duplicate permission", func(s *configbackup.Snapshot) {
			s.Workloads[0].RoutePermissions = append(s.Workloads[0].RoutePermissions, s.Workloads[0].RoutePermissions[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed configbackup.Snapshot
			if err := json.Unmarshal(original, &changed); err != nil {
				t.Fatal(err)
			}
			tc.change(&changed)
			status, body := serviceCall(t, target, "POST", "/api/config/restore", "", "", changed, true)
			if status != 400 {
				t.Fatalf("validation: %d %s", status, body)
			}
			if bytes.Contains(body, []byte("private-backend-secret")) {
				t.Fatal("diagnostic exposed secret")
			}
			assertEmptyNotificationConfig(t, target)
		})
	}
	// All routes and sources have been inserted when the final receipt fails.
	if _, err = target.store.DB().Exec(`CREATE TRIGGER fail_route_restore BEFORE INSERT ON configuration_restores BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := serviceCall(t, target, "POST", "/api/config/restore", "", "", snapshot, true)
	if status != 503 {
		t.Fatalf("rollback: %d %s", status, body)
	}
	assertEmptyNotificationConfig(t, target)
	if _, err = target.store.DB().Exec(`DROP TRIGGER fail_route_restore`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		status, body = serviceCall(t, target, "POST", "/api/config/restore", "", "", snapshot, true)
		var receipt configbackup.Receipt
		if err = json.Unmarshal(body, &receipt); err != nil {
			t.Fatal(err)
		}
		if status != 200 || !receipt.ConfigurationCommitted || receipt.BusinessRoutes != 1 || receipt.Workloads != 1 || receipt.Replayed != (i == 1) || receipt.ExternalHealth != "not_verified" {
			t.Fatalf("receipt: %d %s", status, body)
		}
	}
	status, body = serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
	if status != 200 {
		t.Fatalf("export: %d %s", status, body)
	}
	restored, err := configbackup.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	expected, _, _ := snapshot.Canonical()
	actual, _, _ := restored.Canonical()
	if !bytes.Equal(expected, actual) {
		t.Fatal("backend fields or operation grants changed")
	}
	// A different immutable route key is a configuration conflict, never a rename.
	snapshot.BusinessRoutes[0].RouteKey = "renamed"
	for i := range snapshot.Workloads[0].RoutePermissions {
		snapshot.Workloads[0].RoutePermissions[i].RouteKey = "renamed"
	}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", snapshot, true)
	if status != 409 {
		t.Fatalf("route key overwrite: %d", status)
	}
}
