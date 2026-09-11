package tests

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
)

func configScript(t *testing.T, h *e2eHarness, action, file string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("python3", "../scripts/config-backup.py", action, "--url", h.ts.URL, "--file", file)
	cmd.Env = append(os.Environ(), "BOTMUX_ADMIN_COOKIE="+auth.SessionCookieName+"="+h.session)
	return cmd.CombinedOutput()
}

func TestE2E_ConfigScriptRoundTrip(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	target := setupE2E(t, withHTTPServer())
	const token = "960001:synthetic-backup"
	id := source.AddBot(models.BotConfig{Name: "Backup Bot", Token: token, BotUsername: "backup_bot", ManageEnabled: true, LongPollEnabled: true, PollingTimeout: 1, SecretToken: "synthetic-webhook"})
	accounts, err := source.store.GetBotAccounts()
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.store.AddTelegramDestination(models.TelegramDestination{Name: "Large chat", BotAccountID: accounts[0].ID, ChatID: -9007199254740993, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	target.fake.RegisterBot(token, "backup_bot", 960001)
	// Advance target IDs without leaving business configuration behind.
	unused := target.AddBot(models.BotConfig{Name: "Removed", Token: "960009:synthetic-removed"})
	if err := target.store.DeleteBotConfig(unused); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "botmux.json")
	if output, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, output)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte(token)) || !bytes.Contains(before, []byte("-9007199254740993")) {
		t.Fatal("backup missing token or exact chat ID")
	}
	source.store.IncrementBotForwarded(id)
	if output, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("repeat export: %v %s", err, output)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("runtime activity changed backup")
	}
	for i := 0; i < 2; i++ {
		output, err := configScript(t, target, "restore", file)
		if err != nil {
			t.Fatalf("restore: %v %s", err, output)
		}
		if bytes.Contains(output, []byte(token)) {
			t.Fatal("script leaked token")
		}
	}
	bots, err := target.store.GetBotConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].ID == id || bots[0].Token != token || bots[0].SecretToken != "synthetic-webhook" || !target.proxy.IsRunning(bots[0].ID) {
		t.Fatal("restored bot identity/configuration/runtime mismatch")
	}
	destinations, err := target.store.GetTelegramDestinations()
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 || destinations[0].ChatID != -9007199254740993 {
		t.Fatal("chat target lost precision")
	}
	exported := filepath.Join(t.TempDir(), "target.json")
	if output, err := configScript(t, target, "export", exported); err != nil {
		t.Fatalf("target export: %v %s", err, output)
	}
	restored, _ := os.ReadFile(exported)
	if !bytes.Equal(before, restored) {
		t.Fatal("cross-instance snapshot changed")
	}
}

func TestE2E_ConfigValidationAndAuthorization(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	source.AddBot(models.BotConfig{Name: "Validation", Token: "960002:synthetic-validate"})
	status, raw := serviceCall(t, source, "GET", "/api/config/export", "", "", nil, true)
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	target := setupE2E(t, withHTTPServer())
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"version", func(s map[string]any) { s["schema_version"] = 2 }},
		{"unknown", func(s map[string]any) { s["secret-field-do-not-echo"] = true }},
		{"missing secret", func(s map[string]any) { delete(s["bots"].([]any)[0].(map[string]any), "token") }},
		{"missing boolean", func(s map[string]any) { delete(s["bots"].([]any)[0].(map[string]any), "disabled") }},
		{"null boolean", func(s map[string]any) { s["bots"].([]any)[0].(map[string]any)["disabled"] = nil }},
		{"dangling reference", func(s map[string]any) {
			s["destinations"] = []any{map[string]any{"ref": "d1", "bot_ref": "missing", "name": "X", "chat_id": "-1001", "status": "active"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot map[string]any
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			tc.change(snapshot)
			status, response := serviceCall(t, target, "POST", "/api/config/restore", "", "", snapshot, true)
			if status != 400 {
				t.Fatalf("invalid snapshot status=%d", status)
			}
			if bytes.Contains(response, []byte("secret-field-do-not-echo")) {
				t.Fatal("error echoed arbitrary input")
			}
			bots, _ := target.store.GetBotConfigs()
			if len(bots) != 0 {
				t.Fatal("invalid snapshot left partial Bots")
			}
		})
	}
	var snapshot map[string]any
	_ = json.Unmarshal(raw, &snapshot)
	snapshot["business_routes"] = []any{map[string]any{"route_key": "future"}}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", snapshot, true)
	if status != 422 {
		t.Fatalf("unsupported rules: %d", status)
	}
	for _, path := range []string{"/api/config/export", "/api/config/restore"} {
		status, _ = serviceCall(t, target, "POST", path, "synthetic-workload", "", snapshot, false)
		if status != 401 {
			t.Fatalf("workload credential accepted: %d", status)
		}
	}
	userID, err := target.store.CreateUser("ordinary", "unused", "Ordinary", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.store.CreateSession("ordinary-session", userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	admin := target.session
	target.session = "ordinary-session"
	status, _ = serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
	target.session = admin
	if status != 403 {
		t.Fatalf("ordinary user export: %d", status)
	}
	// A previously applied snapshot is not an authorization to overwrite edits.
	var original any
	_ = json.Unmarshal(raw, &original)
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", original, true)
	if status != 200 {
		t.Fatalf("valid restore: %d", status)
	}
	bots, _ := target.store.GetBotConfigs()
	bots[0].Name = "Edited"
	if err := target.store.UpdateBotConfig(bots[0]); err != nil {
		t.Fatal(err)
	}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", original, true)
	if status != 409 {
		t.Fatalf("changed target: %d", status)
	}
	for _, path := range []string{"/api/bots", "/api/gateway/v1/bot-accounts"} {
		_, listed := serviceCall(t, target, "GET", path, "", "", nil, true)
		if bytes.Contains(listed, []byte("960002:synthetic-validate")) {
			t.Fatal("ordinary list leaked token")
		}
	}
}

func TestE2E_ConfigDownloadPreservesPreviousFile(t *testing.T) {
	for _, scenario := range []string{"error", "invalid JSON", "truncated", "redirect", "error object"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "error":
					w.WriteHeader(503)
					_, _ = w.Write([]byte("synthetic-secret-error"))
				case "invalid JSON":
					_, _ = w.Write([]byte(`{"schema_version":`))
				case "truncated":
					w.Header().Set("Content-Length", "999")
					_, _ = w.Write([]byte(`{}`))
				case "redirect":
					http.Redirect(w, r, "/somewhere", 302)
				case "error object":
					_, _ = w.Write([]byte(`{"error":"synthetic-secret-error"}`))
				}
			}))
			defer server.Close()
			h := &e2eHarness{ts: server, session: "synthetic-admin"}
			file := filepath.Join(t.TempDir(), "botmux.json")
			if err := os.WriteFile(file, []byte("previous backup"), 0600); err != nil {
				t.Fatal(err)
			}
			output, err := configScript(t, h, "export", file)
			if err == nil {
				t.Fatal("invalid download succeeded")
			}
			if bytes.Contains(output, []byte("synthetic-secret-error")) {
				t.Fatal("terminal leaked response")
			}
			saved, _ := os.ReadFile(file)
			if string(saved) != "previous backup" {
				t.Fatal("failed download replaced old backup")
			}
		})
	}
}

func TestE2E_ConfigLegacyAliasesAndDisabledBots(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	// Reproduce deployed plaintext legacy accounts before the compatibility migration.
	_, err := source.store.DB().Exec(`INSERT INTO bots(id,name,token,manage_enabled,proxy_enabled,disabled,secret_token,long_poll_enabled,polling_timeout) VALUES(41,'Disabled','960041:synthetic-disabled',1,1,1,'synthetic-webhook-disabled',1,42);
 INSERT INTO gateway_bot_accounts(id,name,username,token,created_at,updated_at) VALUES
 (71,'First alias','legacy','960042:synthetic-legacy','',''),(72,'Second alias','legacy','960042:synthetic-legacy','','');
 INSERT INTO gateway_telegram_destinations(id,name,bot_account_id,chat_id,status,created_at,updated_at) VALUES
 (81,'A',71,-1009001,'active','',''),(82,'Alias A',72,-1009001,'disabled','',''),(83,'B',72,-1009002,'active','','')`)
	if err != nil {
		t.Fatal(err)
	}
	reopenConfigHarness(t, source)
	accounts, err := source.store.GetBotAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.NativeBotID == 41 {
			if _, err := source.store.AddTelegramDestination(models.TelegramDestination{Name: "Disabled Bot same group", BotAccountID: a.ID, ChatID: -1009001, Status: "disabled"}); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	target := setupE2E(t, withHTTPServer())
	file := filepath.Join(t.TempDir(), "legacy.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export legacy: %v %s", err, out)
	}
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("restore legacy: %v %s", err, out)
	}
	bots, err := target.store.GetBotConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(bots) != 2 {
		t.Fatalf("legacy aliases duplicated Bots: %d", len(bots))
	}
	for _, b := range bots {
		if target.proxy.IsRunning(b.ID) {
			t.Fatal("disabled or send-only Bot started")
		}
		if b.Token == "960041:synthetic-disabled" && (!b.Disabled || !b.ManageEnabled || !b.ProxyEnabled || !b.LongPollEnabled || b.PollingTimeout != 42 || b.SecretToken != "synthetic-webhook-disabled") {
			t.Fatal("disabled Bot configuration lost")
		}
	}
	destinations, err := target.store.GetTelegramDestinations()
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 4 {
		t.Fatal("legacy destination aliases merged")
	}
	var nativeID int64
	for _, d := range destinations {
		a, err := target.store.GetBotAccount(d.BotAccountID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Name == "Disabled Bot same group" {
			if a.Token != "960041:synthetic-disabled" {
				t.Fatal("same group across Bots was merged")
			}
			continue
		}
		if nativeID == 0 {
			nativeID = a.NativeBotID
		}
		if nativeID != a.NativeBotID {
			t.Fatal("aliases have independent Bot identity")
		}
	}
	if len(target.fake.Requests()) != 0 {
		t.Fatal("restoring disabled/send-only Bots contacted Telegram")
	}
	second := filepath.Join(t.TempDir(), "again.json")
	if out, err := configScript(t, target, "export", second); err != nil {
		t.Fatalf("export target: %v %s", err, out)
	}
	a, _ := os.ReadFile(file)
	b, _ := os.ReadFile(second)
	if !bytes.Equal(a, b) {
		t.Fatal("legacy alias snapshot changed on restore")
	}
}

func TestE2E_ConfigRuntimeFailureCanRetry(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	const token = "960051:synthetic-retry"
	source.AddBot(models.BotConfig{Name: "Retry", Token: token, ManageEnabled: true, PollingTimeout: 1})
	target := setupE2E(t, withHTTPServer())
	file := filepath.Join(t.TempDir(), "retry.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	// Unknown Telegram identity causes runtime loading to fail after commit.
	out, err := configScript(t, target, "restore", file)
	if err == nil || !bytes.Contains(out, []byte("Configuration committed: 1 Bots")) || !bytes.Contains(out, []byte("runtime loaded: False")) {
		t.Fatalf("missing partial-runtime receipt: %v %s", err, out)
	}
	var snapshot struct {
		Bots []struct {
			Ref string `json:"ref"`
		} `json:"bots"`
	}
	raw, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(snapshot.Bots[0].Ref)) {
		t.Fatal("runtime failure did not identify Bot ref")
	}
	bots, err := target.store.GetBotConfigs()
	if err != nil || len(bots) != 1 {
		t.Fatal("runtime failure lost committed configuration")
	}
	target.fake.RegisterBot(token, "retry_bot", 960051)
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry failed: %v %s", err, out)
	}
	if !target.proxy.IsRunning(bots[0].ID) {
		t.Fatal("retry did not load runtime")
	}
	if len(target.fake.RequestsFor("sendMessage")) != 0 {
		t.Fatal("restore sent a test notification")
	}
}

func TestE2E_ConfigRuntimeErrorsDoNotLogSecrets(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	const token = "960061:synthetic-log-secret"
	source.AddBot(models.BotConfig{Name: "Log check", Token: token, ProxyEnabled: true, PollingTimeout: 1, BackendURL: "https://synthetic-user:synthetic-url-secret@example.invalid"})
	target := setupE2E(t, withHTTPServer())
	// Refused connection puts the credential-bearing request URL in Go's error.
	target.fake.server.Close()
	file := filepath.Join(t.TempDir(), "log.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	logs, err := os.CreateTemp(t.TempDir(), "logs")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	previous := log.Writer()
	log.SetOutput(logs)
	defer log.SetOutput(previous)
	out, err := configScript(t, target, "restore", file)
	if err != nil {
		t.Fatalf("proxy runtime load: %v %s", err, out)
	}
	target.Eventually(func() bool { bots, _ := target.store.GetBotConfigs(); return len(bots) == 1 && bots[0].LastError != "" }, 2*time.Second, "poll failure should be recorded")
	target.proxy.StopAll()
	target.proxy.Start()
	raw, err := os.ReadFile(logs.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, "synthetic-url-secret"} {
		if bytes.Contains(raw, []byte(secret)) || bytes.Contains(out, []byte(secret)) {
			t.Fatal("runtime failure leaked a secret")
		}
	}
}

func TestE2E_ConfigTransactionRollbackAndRestartReceipt(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	source.AddBot(models.BotConfig{Name: "Durable", Token: "960071:synthetic-durable"})
	target := setupE2E(t, withHTTPServer())
	file := filepath.Join(t.TempDir(), "durable.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	// Fault injection at the durable receipt boundary must roll back the Bots too.
	_, err := target.store.DB().Exec(`CREATE TRIGGER fail_restore_receipt BEFORE INSERT ON configuration_restores BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configScript(t, target, "restore", file); err == nil {
		t.Fatal("injected transaction failure succeeded")
	}
	bots, _ := target.store.GetBotConfigs()
	accounts, _ := target.store.GetBotAccounts()
	if len(bots)+len(accounts) != 0 {
		t.Fatal("receipt failure left partial configuration")
	}
	if _, err := target.store.DB().Exec(`DROP TRIGGER fail_restore_receipt`); err != nil {
		t.Fatal(err)
	}
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry transaction: %v %s", err, out)
	}
	reopenConfigHarness(t, target)
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry after restart: %v %s", err, out)
	}
	raw, _ := os.ReadFile(file)
	status, receipt := serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	if status != 200 || !bytes.Contains(receipt, []byte(`"replayed":true`)) {
		t.Fatal("durable receipt not reused")
	}
	bots, _ = target.store.GetBotConfigs()
	if len(bots) != 1 {
		t.Fatal("restart retry duplicated Bot")
	}
}

func TestE2E_ConfigRejectsUnsupportedSourceConfiguration(t *testing.T) {
	for _, kind := range []string{"conditional routes", "services", "subscriptions", "business routes"} {
		t.Run(kind, func(t *testing.T) {
			h := setupE2E(t, withHTTPServer())
			file := filepath.Join(t.TempDir(), "backup.json")
			if out, err := configScript(t, h, "export", file); err != nil {
				t.Fatalf("empty export: %v %s", err, out)
			}
			before, _ := os.ReadFile(file)
			// Legacy fixtures may contain disabled and orphaned records; neither may be
			// silently omitted from a snapshot advertised as complete.
			queries := map[string]string{
				"conditional routes": `INSERT INTO routes(source_bot_id,target_bot_id,enabled) VALUES(99,99,0)`,
				"services":           `INSERT INTO gateway_notification_services(workload_id,fingerprint,display_name,last_received_at) VALUES(-1234,'orphan','Orphan','')`,
				"subscriptions":      `INSERT INTO gateway_service_subscriptions(service_id,destination_id,active,created_at) VALUES(99,99,0,'')`,
				"business routes":    `INSERT INTO gateway_business_routes(route_key,display_name,bot_account_id,destination_id,enabled,created_at,updated_at) VALUES('disabled','Disabled',99,99,0,'','')`,
			}
			if _, err := h.store.DB().Exec(queries[kind]); err != nil {
				t.Fatal(err)
			}
			if _, err := configScript(t, h, "export", file); err == nil {
				t.Fatal("unsupported configuration exported")
			}
			after, _ := os.ReadFile(file)
			if !bytes.Equal(before, after) {
				t.Fatal("unsupported export overwrote backup")
			}
		})
	}
}

func TestE2E_ConfigScriptReportsSafeFieldLocation(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	source.AddBot(models.BotConfig{Name: "Location", Token: "960081:synthetic-location"})
	target := setupE2E(t, withHTTPServer())
	file := filepath.Join(t.TempDir(), "invalid.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	raw, _ := os.ReadFile(file)
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	delete(snapshot["bots"].([]any)[0].(map[string]any), "token")
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := configScript(t, target, "restore", file)
	if err == nil || !bytes.Contains(out, []byte("snapshot.bots[0].token")) {
		t.Fatalf("script must identify invalid field: %v %s", err, out)
	}
}

// Reopen through real startup migrations while keeping the persisted admin session.
func reopenConfigHarness(t *testing.T, h *e2eHarness) {
	t.Helper()
	var path string
	if err := h.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	h.proxy.StopAll()
	h.ts.Close()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h.store = reopened
	h.proxy = proxy.NewManager(reopened, h.fake.URL())
	t.Cleanup(h.proxy.StopAll)
	h.server = server.NewServer(reopened, h.proxy)
	h.ts = httptest.NewServer(h.server.BuildMux())
	t.Cleanup(h.ts.Close)
}

func TestE2E_ConfigRetryAfterLostHTTPResponse(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	source.AddBot(models.BotConfig{Name: "Lost response", Token: "960091:synthetic-lost"})
	target := setupE2E(t, withHTTPServer())
	var dropped atomic.Bool
	mux := target.server.BuildMux()
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config/restore" && dropped.CompareAndSwap(false, true) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, r)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer wrapper.Close()
	target.ts = wrapper
	file := filepath.Join(t.TempDir(), "lost.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	if _, err := configScript(t, target, "restore", file); err == nil {
		t.Fatal("lost response appeared successful")
	}
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry lost response: %v %s", err, out)
	}
	bots, _ := target.store.GetBotConfigs()
	if len(bots) != 1 {
		t.Fatal("lost response retry duplicated Bots")
	}
}

func TestE2E_ConfigExportConsistentDuringConfigurationWrites(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	h.AddBot(models.BotConfig{Name: "Before", Token: "960092:synthetic-consistency"})
	accounts, err := h.store.GetBotAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AddTelegramDestination(models.TelegramDestination{Name: "Before", BotAccountID: accounts[0].ID, ChatID: -10092, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				finished <- nil
				return
			default:
			}
			name := "Before"
			if i%2 == 0 {
				name = "After"
			}
			tx, err := h.store.DB().Begin()
			if err != nil {
				finished <- err
				return
			}
			if _, err = tx.Exec(`UPDATE bots SET name=?`, name); err == nil {
				_, err = tx.Exec(`UPDATE gateway_telegram_destinations SET name=?`, name)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if err != nil {
				finished <- err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	file := filepath.Join(t.TempDir(), "consistent.json")
	for i := 0; i < 5; i++ {
		if out, err := configScript(t, h, "export", file); err != nil {
			t.Fatalf("concurrent export: %v %s", err, out)
		}
		raw, _ := os.ReadFile(file)
		var snapshot struct {
			Bots         []struct{ Name string }
			Destinations []struct{ Name string }
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Bots[0].Name != snapshot.Destinations[0].Name {
			t.Fatal("export mixed two committed configurations")
		}
	}
}
