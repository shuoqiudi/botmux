package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/configbackup"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

// Build through the same management/store boundaries as the existing migration
// fixtures. Every collection is populated, with one source shared by both APIs.
func fullConfigSource(t *testing.T) *e2eHarness {
	t.Helper()
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	bot, err := f.h.store.GetBotConfig(f.account.NativeBotID)
	if err != nil {
		t.Fatal(err)
	}
	bot.ManageEnabled = true
	bot.PollingTimeout = 1
	if err := f.h.store.UpdateBotConfig(*bot); err != nil {
		t.Fatal(err)
	}
	second := f.h.AddBot(models.BotConfig{Name: "Forward target", Token: "940002:second-secret", ManageEnabled: true, PollingTimeout: 1})
	if _, err := f.h.store.AddRoute(models.Route{SourceBotID: f.account.NativeBotID, TargetBotID: second, TargetChatID: -100940002, ConditionType: "text", ConditionValue: "urgent", Action: "forward", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.store.AddBusinessRoute(models.BusinessRoute{RouteKey: "operations", DisplayName: "Operations", BotAccountID: f.account.ID, DestinationID: f.destination.ID, OutboundEnabled: true, Enabled: true, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	workloads, err := f.h.store.GetGatewayWorkloadsAdmin()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workloads {
		if w.Name == "Subscriptions" {
			if err := f.h.store.GrantGatewayPermission(w.ID, "operations", models.GatewayActionMessagesSend); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f.h
}

func startConfigGateway(t *testing.T, h *e2eHarness) *gateway.Service {
	t.Helper()
	worker := gateway.NewService(h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(h.fake.URL()), "configuration")
	h.server.SetGatewayService(worker)
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	return worker
}

func registerFullConfigBots(h *e2eHarness) {
	h.fake.RegisterBot("940001:subscription-secret", "subscriber", 940001)
	h.fake.RegisterChat("940001:subscription-secret", -100940001, "Operations")
	h.fake.RegisterBot("940002:second-secret", "second", 940002)
	h.fake.RegisterChat("940002:second-secret", -100940002, "Forward target")
}

func TestE2E_ConfigFullGatewayFailureRetry(t *testing.T) {
	source := fullConfigSource(t)
	target := setupE2E(t, withHTTPServer())
	file := filepath.Join(t.TempDir(), "full.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	out, err := configScript(t, target, "restore", file)
	if err == nil || !bytes.Contains(out, []byte("runtime loaded: False")) || !bytes.Contains(out, []byte("gateway_outbound")) {
		t.Fatalf("missing Gateway failure status: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	status, body := serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	var receipt configbackup.Receipt
	if json.Unmarshal(body, &receipt) != nil || status != 200 || !receipt.ConfigurationCommitted || !receipt.Replayed || receipt.RuntimeLoaded || len(receipt.RuntimeFailedRefs) != 2 {
		t.Fatalf("receipt: %d %s", status, body)
	}
	registerFullConfigBots(target)
	worker := startConfigGateway(t, target)
	for i := 0; i < 2; i++ {
		if out, err := configScript(t, target, "restore", file); err != nil {
			t.Fatalf("retry: %v %s", err, out)
		}
	}
	worker.Stop()
	if out, err := configScript(t, target, "restore", file); err == nil || !bytes.Contains(out, []byte("gateway_outbound")) {
		t.Fatalf("stopped Gateway reported loaded: %v %s", err, out)
	}
	if len(target.fake.RequestsFor("sendMessage")) != 0 {
		t.Fatal("restore synthesized notifications")
	}
}

func TestE2E_ConfigFullLostResponseRestartAndBehavior(t *testing.T) {
	source := fullConfigSource(t)
	target := setupE2E(t, withHTTPServer())
	registerFullConfigBots(target)
	file := filepath.Join(t.TempDir(), "full.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	mux := target.server.BuildMux()
	originalServer := target.ts
	target.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, r)
		if recorder.Code != 200 || !bytes.Contains(recorder.Body.Bytes(), []byte(`"configuration_committed":true`)) {
			t.Error("response lost before commit")
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	}))
	defer originalServer.Close()
	if out, err := configScript(t, target, "restore", file); err == nil {
		t.Fatalf("lost response succeeded: %s", out)
	}
	reopenConfigHarness(t, target)
	startConfigGateway(t, target)
	for i := 0; i < 2; i++ {
		if out, err := configScript(t, target, "restore", file); err != nil || !bytes.Contains(out, []byte("replayed: True")) {
			t.Fatalf("durable retry: %v %s", err, out)
		}
	}
	if len(target.fake.RequestsFor("sendMessage")) != 0 {
		t.Fatal("restore replayed history")
	}
	assertConfigExport(t, target, original)
	// All three rule kinds execute after a durable retry, using the original source
	// credential. The original registration idempotency key is intentionally reused.
	for _, call := range []struct {
		path, key string
		payload   map[string]any
	}{
		{"/api/v1/routes/operations/messages", "business", map[string]any{"text": "business"}},
		{"/api/v1/services/notifications", "register", map[string]any{"fingerprint": "dns", "text": "notification"}},
	} {
		status, body := serviceCall(t, target, "POST", call.path, "subscriber-secret", call.key, call.payload, false)
		if status != 202 {
			t.Fatalf("restored API: %d %s", status, body)
		}
	}
	target.fake.EnqueueUpdate("940001:subscription-secret", map[string]any{"update_id": 700, "message": map[string]any{"message_id": 700, "chat": map[string]any{"id": -100940001, "type": "supergroup"}, "text": "urgent"}})
	target.Eventually(func() bool { return len(target.fake.RequestsFor("sendMessage")) == 3 }, 5*time.Second, "all three rules send")
	seen := map[string]bool{}
	for _, request := range target.fake.RequestsFor("sendMessage") {
		var sent struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		if json.Unmarshal(request.body, &sent) != nil {
			form, err := url.ParseQuery(string(request.body))
			if err != nil {
				t.Fatal(err)
			}
			sent.Text = form.Get("text")
			sent.ChatID = parseChatID(nil, request.body)
		}
		switch {
		case sent.Text == "business" || sent.Text == "notification":
			if request.token != "940001:subscription-secret" || sent.ChatID != -100940001 {
				t.Fatal("wrong Gateway recipient")
			}
			seen[sent.Text] = true
		case bytes.Contains([]byte(sent.Text), []byte("urgent")):
			if request.token != "940002:second-secret" || sent.ChatID != -100940002 {
				t.Fatal("wrong forwarding recipient")
			}
			seen["conditional"] = true
		default:
			t.Fatalf("unexpected message: %s", sent.Text)
		}
	}
	if len(seen) != 3 {
		t.Fatal("missing rule behavior or duplicate delivery")
	}
	assertConfigExport(t, target, original)
	bots, err := target.store.GetBotConfigs()
	if err != nil {
		t.Fatal(err)
	}
	var sourceID int64
	for _, bot := range bots {
		if bot.Token == "940001:subscription-secret" {
			sourceID = bot.ID
		}
	}
	routes, err := target.store.GetRoutes(sourceID)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes: %v %v", routes, err)
	}
	routes[0].Description = "Changed in management"
	status, body := serviceCall(t, target, "POST", "/api/routes/update", "", "", routes[0], true)
	if status != 200 {
		t.Fatalf("page edit: %d %s", status, body)
	}
	if out, err := configScript(t, target, "restore", file); err == nil || !bytes.Contains(out, []byte("target_conflict")) {
		t.Fatalf("stale receipt accepted: %v %s", err, out)
	}
	parts := bytes.SplitN(original, []byte(`"conditional_routes":`), 2)
	expected := append(append([]byte{}, parts[0]...), []byte(`"conditional_routes":`)...)
	expected = append(expected, bytes.Replace(parts[1], []byte(`"description": ""`), []byte(`"description": "Changed in management"`), 1)...)
	assertConfigExport(t, target, expected)
}

func assertConfigExport(t *testing.T, h *e2eHarness, want []byte) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "export.json")
	if out, err := configScript(t, h, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("configuration differs from complete expected snapshot")
	}
}

func TestE2E_ConfigFullPersistenceFailuresAreAtomic(t *testing.T) {
	source := fullConfigSource(t)
	file := filepath.Join(t.TempDir(), "full.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	empty, _, err := configbackup.Empty().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"bots", "gateway_telegram_destinations", "routes", "gateway_workload_credentials", "gateway_service_subscriptions", "gateway_business_routes", "gateway_route_permissions", "configuration_restores"} {
		t.Run(table, func(t *testing.T) {
			target := setupE2E(t, withHTTPServer())
			registerFullConfigBots(target)
			startConfigGateway(t, target)
			// Database fault injection at successive persistence boundaries. Assertions
			// remain at the real script/API boundary, including the durable receipt.
			if _, err := target.store.DB().Exec(`CREATE TRIGGER fail_full_restore BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'private-injected-secret'); END`); err != nil {
				t.Fatal(err)
			}
			out, err := configScript(t, target, "restore", file)
			if err == nil || !bytes.Contains(out, []byte("storage_unavailable")) || bytes.Contains(out, []byte("private-injected-secret")) {
				t.Fatalf("failure: %v %s", err, out)
			}
			assertConfigExport(t, target, empty)
			if len(target.fake.Requests()) != 0 {
				t.Fatal("persistence failure loaded Bots or sent traffic")
			}
			if _, err := target.store.DB().Exec(`DROP TRIGGER fail_full_restore`); err != nil {
				t.Fatal(err)
			}
			out, err = configScript(t, target, "restore", file)
			if err != nil || !bytes.Contains(out, []byte("replayed: False")) {
				t.Fatalf("retry retained partial receipt: %v %s", err, out)
			}
			assertConfigExport(t, target, original)
		})
	}
}

func TestE2E_ConfigFullInvalidFilesLeaveNoConfiguration(t *testing.T) {
	source := fullConfigSource(t)
	file := filepath.Join(t.TempDir(), "full.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	empty, _, err := configbackup.Empty().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	target := setupE2E(t, withHTTPServer())
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"unknown version", func(s map[string]any) { s["schema_version"] = 99 }},
		{"unknown field", func(s map[string]any) { s["private-field"] = "private-secret" }},
		{"missing key", func(s map[string]any) { delete(s["bots"].([]any)[0].(map[string]any), "token") }},
		{"overflow chat", func(s map[string]any) {
			s["destinations"].([]any)[0].(map[string]any)["chat_id"] = "9223372036854775808"
		}},
		{"floating chat", func(s map[string]any) {
			s["conditional_routes"].([]any)[0].(map[string]any)["target_chat_id"] = -9007199254740992.0
		}},
		{"route bot missing", func(s map[string]any) {
			s["conditional_routes"].([]any)[0].(map[string]any)["target_bot_ref"] = "missing"
		}},
		{"business destination missing", func(s map[string]any) {
			s["business_routes"].([]any)[0].(map[string]any)["destination_ref"] = "missing"
		}},
		{"subscription source missing", func(s map[string]any) { s["subscriptions"].([]any)[0].(map[string]any)["workload_ref"] = "missing" }},
		{"duplicate effective subscription", func(s map[string]any) {
			original := s["subscriptions"].([]any)[0].(map[string]any)
			duplicate := map[string]any{}
			for k, v := range original {
				duplicate[k] = v
			}
			duplicate["ref"] = "another"
			s["subscriptions"] = append(s["subscriptions"].([]any), duplicate)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed map[string]any
			if err := json.Unmarshal(original, &changed); err != nil {
				t.Fatal(err)
			}
			tc.change(changed)
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			out, err := configScript(t, target, "restore", file)
			if err == nil || !bytes.Contains(out, []byte("invalid_snapshot")) || bytes.Contains(out, []byte("private-secret")) {
				t.Fatalf("validation: %v %s", err, out)
			}
			assertConfigExport(t, target, empty)
			if len(target.fake.Requests()) != 0 {
				t.Fatal("invalid snapshot caused external requests")
			}
		})
	}
}

func TestE2E_ConfigFullConcurrentExportAndActivity(t *testing.T) {
	h := fullConfigSource(t)
	write := func(name string) error {
		tx, err := h.store.DB().Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, statement := range []string{
			`UPDATE bots SET name=?`,
			`UPDATE gateway_telegram_destinations SET name=?`,
			`UPDATE routes SET description=?`,
			`UPDATE gateway_workloads SET name=? WHERE id<>-1234`,
			`UPDATE gateway_notification_services SET display_name=?`,
			`UPDATE gateway_business_routes SET display_name=?`,
		} {
			if _, err := tx.Exec(statement, name); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	snapshots := map[string][]byte{}
	for _, name := range []string{"Before", "After"} {
		if err := write(name); err != nil {
			t.Fatal(err)
		}
		status, raw := serviceCall(t, h, "GET", "/api/config/export", "", "", nil, true)
		if status != 200 {
			t.Fatalf("export: %d", status)
		}
		snapshots[name] = raw
	}
	if !bytes.Equal(bytes.ReplaceAll(snapshots["Before"], []byte("Before"), []byte("After")), snapshots["After"]) {
		t.Fatal("configuration edits produced unrelated differences")
	}
	bots, err := h.store.GetBotConfigs()
	if err != nil {
		t.Fatal(err)
	}
	for _, bot := range bots {
		h.store.IncrementBotForwarded(bot.ID)
		h.store.UpdateBackendHealth(bot.ID, "unhealthy", "2026-09-11T00:00:00Z")
	}
	if _, err := h.store.DB().Exec(`UPDATE gateway_workload_credentials SET last_used_at='2026-09-11T00:00:00Z'; UPDATE gateway_notification_services SET last_received_at='2026-09-11T00:00:00Z'; UPDATE gateway_business_routes SET last_validated_at='2026-09-11T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	assertConfigExport(t, h, snapshots["After"])
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			name := "Before"
			if i%2 == 0 {
				name = "After"
			}
			if err := write(name); err != nil {
				done <- err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	file := filepath.Join(t.TempDir(), "concurrent.json")
	for i := 0; i < 8; i++ {
		if out, err := configScript(t, h, "export", file); err != nil {
			t.Fatalf("export: %v %s", err, out)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, snapshots["Before"]) && !bytes.Equal(raw, snapshots["After"]) {
			t.Fatal("export mixed committed configurations")
		}
	}
}

func TestE2E_ConfigLocalWriteFailurePreservesBackup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux file-size limit")
	}
	source := fullConfigSource(t)
	file := filepath.Join(t.TempDir(), "previous.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("initial export: %v %s", err, out)
	}
	previous, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise a real local write failure, including when tests run as root.
	cmd := exec.Command("python3", "-c", `import resource, signal, runpy; resource.setrlimit(resource.RLIMIT_FSIZE, (16, 16)); signal.signal(signal.SIGXFSZ, signal.SIG_IGN); runpy.run_path('../scripts/config-backup.py', run_name='__main__')`, "export", "--url", source.ts.URL, "--file", file)
	cmd.Env = append(os.Environ(), "BOTMUX_ADMIN_COOKIE="+auth.SessionCookieName+"="+source.session)
	out, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(out, []byte("Configuration operation failed")) || bytes.Contains(out, []byte("subscription-secret")) {
		t.Fatalf("write failure: %v %s", err, out)
	}
	current, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, previous) {
		t.Fatal("local write failure replaced complete backup")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary secret file not cleaned: %v %v", entries, err)
	}
}
