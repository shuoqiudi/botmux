package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
)

// TestE2E_GatewayManagement is deliberately serial: one externally observable
// flow covers the SPA, HTTP contract, SQLite persistence and fake Telegram.
func TestE2E_GatewayManagement(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	const token = "900001:gateway-secret-value"
	const chatID int64 = -100900001
	h.fake.RegisterBot(token, "gateway_test_bot", 900001)
	h.fake.RegisterChat(token, chatID, "Gateway test group")

	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, h.ts.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(token)) {
			t.Fatal("gateway response exposed a write-only Telegram credential")
		}
		return resp.StatusCode, data
	}

	status, page := call(http.MethodGet, "/", nil)
	if status != 200 || !bytes.Contains(page, []byte("businessRoutesModal")) || !bytes.Contains(page, []byte("businessRouteSetupModal")) {
		t.Fatal("Business Routes management view is missing from the SPA")
	}

	setup := map[string]any{
		"bot_account": map[string]any{"name": "Management Bot", "token": token},
		"destination": map[string]any{"name": "Management Group", "chat_id": chatID},
		"route": map[string]any{
			"route_key": "it_manage", "display_name": "IT Manage", "inbound_enabled": true,
			"inbound_backend_url": "http://it-manage/telegram", "outbound_enabled": true,
			"allowed_callers": []string{"it_manage"}, "enabled": true,
		},
	}
	status, body := call(http.MethodPost, "/api/gateway/v1/routes/setup", setup)
	if status != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", status, body)
	}
	var created models.BusinessRoute
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.RouteKey != "it_manage" || created.Path != "/api/v1/routes/it_manage/messages" || created.Status != "active" || created.Revision != 1 {
		t.Fatalf("unexpected created route: %+v", created)
	}
	if !created.InboundEnabled || !created.OutboundEnabled {
		t.Fatal("route does not expose both direction states")
	}
	nativeBotIDs, err := h.store.NativeBotIDsForGatewayAccount(created.BotAccountID)
	if err != nil || len(nativeBotIDs) != 1 {
		t.Fatalf("Business Route setup did not create exactly one native polling owner: ids=%v err=%v", nativeBotIDs, err)
	}
	if !h.proxy.IsRunning(nativeBotIDs[0]) || h.proxy.GetManagedBot(nativeBotIDs[0]) == nil {
		t.Fatal("Business Route setup did not bind and start its managed native bot")
	}
	const rolledBackToken = "900002:rolled-back-secret"
	h.fake.RegisterBot(rolledBackToken, "rolled_back_bot", 900002)
	h.fake.RegisterChat(rolledBackToken, chatID-1, "Rolled back group")
	rollbackSetup := map[string]any{
		"bot_account": map[string]any{"name": "Must roll back", "token": rolledBackToken},
		"destination": map[string]any{"name": "Must roll back", "chat_id": chatID - 1},
		"route": map[string]any{
			"route_key": "it_manage", "display_name": "Duplicate", "outbound_enabled": true, "enabled": true,
		},
	}
	status, _ = call(http.MethodPost, "/api/gateway/v1/routes/setup", rollbackSetup)
	if status != http.StatusConflict {
		t.Fatalf("conflicting setup status=%d, want 409", status)
	}
	if _, err := h.store.GetBotConfigByToken(rolledBackToken); err == nil {
		t.Fatal("failed Business Route setup left a native polling owner behind")
	}
	accountsAfterRollback, err := h.store.GetBotAccounts()
	if err != nil || len(accountsAfterRollback) != 1 {
		t.Fatalf("failed Business Route setup was not atomic: accounts=%v err=%v", accountsAfterRollback, err)
	}

	status, accountsBody := call(http.MethodGet, "/api/gateway/v1/bot-accounts", nil)
	if status != 200 || bytes.Contains(accountsBody, []byte(`"token"`)) || !bytes.Contains(accountsBody, []byte(`"token_set":true`)) {
		t.Fatalf("bot credential is not write-only: %s", accountsBody)
	}

	// Insert an unreachable candidate to prove activation validation happens
	// before the current active revision is replaced.
	badDestinationID, err := h.store.AddTelegramDestination(models.TelegramDestination{
		Name: "Unreachable group", BotAccountID: created.BotAccountID, ChatID: -100999999,
		Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	failedUpdate := map[string]any{
		"route_key": "it_manage", "display_name": "Should not activate", "bot_account_id": created.BotAccountID,
		"destination_id": badDestinationID, "inbound_enabled": true, "inbound_backend_url": "http://it-manage/telegram",
		"outbound_enabled": true, "allowed_callers": []string{"it_manage"}, "enabled": true,
	}
	status, _ = call(http.MethodPut, "/api/gateway/v1/routes/it_manage", failedUpdate)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("unreachable destination update status=%d, want 422", status)
	}
	stored, err := h.store.GetBusinessRoute("it_manage")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DestinationID != created.DestinationID || stored.DisplayName != "IT Manage" || stored.Revision != 1 {
		t.Fatalf("failed validation replaced active config: %+v", stored)
	}

	immutable := failedUpdate
	immutable["route_key"] = "changed_key"
	immutable["destination_id"] = created.DestinationID
	status, _ = call(http.MethodPut, "/api/gateway/v1/routes/it_manage", immutable)
	if status != http.StatusConflict {
		t.Fatalf("route_key mutation status=%d, want 409", status)
	}

	update := map[string]any{
		"route_key": "it_manage", "display_name": "IT Manage approvals", "bot_account_id": created.BotAccountID,
		"destination_id": created.DestinationID, "inbound_enabled": true, "inbound_backend_url": "http://it-manage/telegram",
		"outbound_enabled": true, "allowed_callers": []string{"it_manage"}, "enabled": true,
	}
	status, body = call(http.MethodPut, "/api/gateway/v1/routes/it_manage", update)
	if status != 200 {
		t.Fatalf("valid update status=%d body=%s", status, body)
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.RouteKey != "it_manage" || created.Revision != 2 || created.DisplayName != "IT Manage approvals" {
		t.Fatalf("stable update mismatch: %+v", created)
	}

	status, body = call(http.MethodPost, "/api/gateway/v1/routes/it_manage/test", map[string]string{"text": "gateway test"})
	if status != 200 || !bytes.Contains(body, []byte(`"status":"sent"`)) {
		t.Fatalf("test message status=%d body=%s", status, body)
	}
	sends := h.fake.RequestsFor("sendMessage")
	var sentPayload struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if len(sends) == 1 {
		_ = json.Unmarshal(sends[0].body, &sentPayload)
	}
	if len(sends) != 1 || sentPayload.ChatID != chatID || sentPayload.Text != "gateway test" {
		t.Fatal("test message did not resolve the configured Telegram destination")
	}

	// The legacy bot list is also redacted after write.
	legacyBotID, err := h.store.AddBotConfig(models.BotConfig{Name: "legacy", Token: token, SecretToken: "backend-secret"})
	if err != nil {
		t.Fatal(err)
	}
	status, body = call(http.MethodGet, "/api/bots", nil)
	if status != 200 || strings.Contains(string(body), "backend-secret") || strings.Contains(string(body), token) {
		t.Fatal("legacy bot response exposed a credential")
	}
	status, body = call(http.MethodPost, "/api/bots/toggle-disabled?id="+strconv.FormatInt(legacyBotID, 10), nil)
	if status != 200 || strings.Contains(string(body), "backend-secret") ||
		!bytes.Contains(body, []byte(`"token_set":true`)) || !bytes.Contains(body, []byte(`"secret_token_set":true`)) {
		t.Fatalf("legacy bot mutation response exposed or omitted credential state: %s", body)
	}
}
