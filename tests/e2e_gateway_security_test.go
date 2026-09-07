package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

func TestGatewayCredentialsEncryptedAndMigrated(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	dbPath := filepath.Join(t.TempDir(), "secure.db")
	st, err := store.NewStoreWithSecretKey(dbPath, key)
	if err != nil {
		t.Fatal(err)
	}
	const token = "990001:full-telegram-secret"
	const backend = "gateway-to-backend-secret"
	if _, err := st.AddBotConfig(models.BotConfig{Name: "native", Token: token, SecretToken: backend}); err != nil {
		t.Fatal(err)
	}
	accountID, err := st.AddBotAccount(models.BotAccount{Name: "gateway", Username: "gateway_bot", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	destinationID, err := st.AddTelegramDestination(models.TelegramDestination{Name: "target", BotAccountID: accountID, ChatID: -100990001})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBusinessRoute(models.BusinessRoute{RouteKey: "secure", DisplayName: "Secure", BotAccountID: accountID, DestinationID: destinationID, InboundEnabled: true, InboundBackendURL: "https://backend.invalid/updates", InboundBackendToken: backend, OutboundEnabled: true, Enabled: true, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"bots", "gateway_bot_accounts", "gateway_business_routes", "gateway_route_revisions"} {
		var sqlText string
		if err := st.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&sqlText); err != nil {
			t.Fatal(err)
		}
	}
	var nativeToken, nativeBackend, gatewayToken, routeBackend, tokenCipher, backendCipher string
	if err := st.DB().QueryRow(`SELECT token,secret_token,token_ciphertext,secret_token_ciphertext FROM bots LIMIT 1`).Scan(&nativeToken, &nativeBackend, &tokenCipher, &backendCipher); err != nil {
		t.Fatal(err)
	}
	if nativeToken != "" || nativeBackend != "" || tokenCipher == "" || backendCipher == "" {
		t.Fatal("native credentials were not migrated to ciphertext")
	}
	if err := st.DB().QueryRow(`SELECT token,token_ciphertext FROM gateway_bot_accounts WHERE id=?`, accountID).Scan(&gatewayToken, &tokenCipher); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT inbound_backend_token,inbound_backend_token_ciphertext FROM gateway_business_routes WHERE route_key='secure'`).Scan(&routeBackend, &backendCipher); err != nil {
		t.Fatal(err)
	}
	if gatewayToken != "" || routeBackend != "" || strings.Contains(tokenCipher, token) || strings.Contains(backendCipher, backend) {
		t.Fatal("Gateway credentials are readable at rest")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.NewStoreWithSecretKey(dbPath, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	account, err := reopened.GetBotAccount(accountID)
	if err != nil || account.Token != token {
		t.Fatalf("encrypted token was not usable after restart: %v", err)
	}
}

func TestE2E_GatewayRotationPermissionsAndAudit(t *testing.T) {
	h := setupE2E(t)
	const oldToken = "990101:old-super-secret"
	const newToken = "990102:new-super-secret"
	const backendSecret = "backend-delivery-secret"
	const chatID int64 = -100990101
	h.fake.RegisterBot(oldToken, "old_bot", 990101)
	h.fake.RegisterChat(oldToken, chatID, "Old destination")
	h.fake.RegisterBot(newToken, "new_bot", 990102)
	h.fake.RegisterChat(newToken, chatID, "Old destination")
	h.ts = httptest.NewServer(h.server.BuildMux())
	t.Cleanup(h.ts.Close)

	adminCall := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, h.ts.URL+path, reader)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	status, body := adminCall(http.MethodPost, "/api/gateway/v1/routes/setup", map[string]any{"bot_account": map[string]any{"name": "Secure bot", "token": oldToken}, "destination": map[string]any{"name": "Secure chat", "chat_id": chatID}, "route": map[string]any{"route_key": "secure_ops", "display_name": "Secure ops", "inbound_enabled": true, "inbound_backend_url": "https://backend.invalid/update", "inbound_backend_token": backendSecret, "outbound_enabled": true, "enabled": true}})
	if status != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", status, body)
	}
	var route models.BusinessRoute
	if err := json.Unmarshal(body, &route); err != nil {
		t.Fatal(err)
	}
	account, err := h.store.GetBotAccount(route.BotAccountID)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := h.store.GetTelegramDestination(route.DestinationID)
	if err != nil {
		t.Fatal(err)
	}

	status, _ = adminCall(http.MethodPut, "/api/gateway/v1/bot-accounts/"+itoa(account.ID), map[string]any{"name": "Secure bot", "token": "invalid-token", "expected_revision": account.Revision})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid rotation status=%d", status)
	}
	unchanged, _ := h.store.GetBotAccount(account.ID)
	currentRoute, _ := h.store.GetBusinessRoute(route.RouteKey)
	if unchanged.Token != oldToken || currentRoute.Revision != route.Revision {
		t.Fatal("failed token validation changed active revision")
	}

	status, _ = adminCall(http.MethodPut, "/api/gateway/v1/destinations/"+itoa(destination.ID), map[string]any{"name": "bad", "bot_account_id": account.ID, "chat_id": -100999999, "expected_revision": destination.Revision})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid migration status=%d", status)
	}
	unchangedDestination, _ := h.store.GetTelegramDestination(destination.ID)
	if unchangedDestination.ChatID != chatID || unchangedDestination.Revision != destination.Revision {
		t.Fatal("failed chat validation changed destination")
	}

	status, body = adminCall(http.MethodPut, "/api/gateway/v1/bot-accounts/"+itoa(account.ID), map[string]any{"name": "Secure bot", "token": newToken, "expected_revision": account.Revision})
	if status != http.StatusOK {
		t.Fatalf("rotation status=%d body=%s", status, body)
	}
	currentRoute, _ = h.store.GetBusinessRoute(route.RouteKey)
	status, _ = adminCall(http.MethodPut, "/api/gateway/v1/routes/"+route.RouteKey, map[string]any{"route_key": route.RouteKey, "display_name": route.DisplayName, "bot_account_id": route.BotAccountID, "destination_id": route.DestinationID, "inbound_enabled": true, "inbound_backend_url": "https://backend.invalid/update", "outbound_enabled": false, "enabled": true, "expected_revision": currentRoute.Revision})
	if status != http.StatusOK {
		t.Fatalf("direction disable status=%d", status)
	}
	disabled, _ := h.store.GetBusinessRoute(route.RouteKey)
	if !disabled.InboundEnabled || disabled.OutboundEnabled || !disabled.Enabled {
		t.Fatal("route directions cannot be disabled independently")
	}
	status, _ = adminCall(http.MethodPut, "/api/gateway/v1/routes/"+route.RouteKey, map[string]any{"route_key": route.RouteKey, "display_name": route.DisplayName, "bot_account_id": route.BotAccountID, "destination_id": route.DestinationID, "inbound_enabled": true, "inbound_backend_url": "https://backend.invalid/update", "outbound_enabled": true, "enabled": true, "expected_revision": currentRoute.Revision})
	if status != http.StatusConflict {
		t.Fatalf("stale revision status=%d", status)
	}

	status, body = adminCall(http.MethodPost, "/api/gateway/v1/workloads", map[string]any{"name": "security-test", "credential_name": "primary"})
	if status != http.StatusCreated {
		t.Fatalf("workload status=%d body=%s", status, body)
	}
	var created struct {
		Workload   models.GatewayWorkloadAdmin `json:"workload"`
		Credential string                      `json:"credential"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Credential, "gwk_") {
		t.Fatal("workload credential was not issued")
	}
	status, body = adminCall(http.MethodGet, "/api/gateway/v1/workloads", nil)
	if status != http.StatusOK || bytes.Contains(body, []byte(created.Credential)) {
		t.Fatal("workload credential can be read back")
	}
	status, body = adminCall(http.MethodPut, "/api/gateway/v1/workloads/"+itoa(created.Workload.ID)+"/permissions", map[string]any{"expected_revision": created.Workload.Revision, "permissions": []map[string]any{{"route_key": route.RouteKey, "action": models.GatewayActionMessagesSend}}})
	if status != http.StatusOK {
		t.Fatalf("grant status=%d body=%s", status, body)
	}
	var granted models.GatewayWorkloadAdmin
	_ = json.Unmarshal(body, &granted)
	service := gateway.NewService(h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(h.fake.URL()), "")
	h.server.SetGatewayService(service)
	businessCall := func(path, credential string, payload any) (int, []byte) {
		raw, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, h.ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Idempotency-Key", "security-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	status, _ = businessCall("/api/v1/routes/"+route.RouteKey+"/callbacks/callback-1/answer", created.Credential, map[string]any{"text": "ok"})
	if status != http.StatusForbidden {
		t.Fatalf("ungranted action status=%d", status)
	}
	status, _ = businessCall("/api/v1/routes/"+route.RouteKey+"/messages", created.Credential, map[string]any{"text": "safe", "chat_id": chatID})
	if status != http.StatusBadRequest {
		t.Fatalf("physical override status=%d", status)
	}

	status, body = adminCall(http.MethodPost, "/api/gateway/v1/workloads/"+itoa(created.Workload.ID)+"/credentials/rotate", map[string]any{"expected_revision": granted.Revision, "name": "next"})
	if status != http.StatusOK {
		t.Fatalf("credential rotation status=%d body=%s", status, body)
	}
	var rotated struct {
		Credential string `json:"credential"`
	}
	_ = json.Unmarshal(body, &rotated)
	status, _ = businessCall("/api/v1/routes/"+route.RouteKey+"/callbacks/callback-2/answer", created.Credential, map[string]any{"text": "old"})
	if status != http.StatusUnauthorized {
		t.Fatalf("old workload credential status=%d", status)
	}
	status, audit := adminCall(http.MethodGet, "/api/gateway/v1/audit?limit=100", nil)
	if status != http.StatusOK {
		t.Fatalf("audit status=%d", status)
	}
	for _, secret := range []string{oldToken, newToken, backendSecret, created.Credential, rotated.Credential, itoa(chatID)} {
		if bytes.Contains(audit, []byte(secret)) {
			t.Fatalf("audit exposed protected value %q", secret)
		}
	}
	if !bytes.Contains(audit, []byte(`"bot_token.rotate"`)) || !bytes.Contains(audit, []byte(`"permissions.replace"`)) || !bytes.Contains(audit, []byte(`"route.disable"`)) {
		t.Fatalf("required audit events missing: %s", audit)
	}
}

func itoa[T ~int64](value T) string { return strconv.FormatInt(int64(value), 10) }
