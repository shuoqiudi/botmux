package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
)

func serviceCall(t *testing.T, h *e2eHarness, method, path, credential, key string, body any, admin bool) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Idempotency-Key", key)
	if admin {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestE2E_ServiceNotificationRegistration(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id, err := h.store.CreateGatewayWorkload("Monitor", "initial", auth.HashAPIKey("service-secret"))
	if err != nil {
		t.Fatal(err)
	}
	status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": 1, "publish": true, "query": true}, true)
	if status != 200 {
		t.Fatalf("grant: %d %s", status, raw)
	}
	status, raw = serviceCall(t, h, "POST", "/api/v1/services/notifications", "service-secret", "event-1", map[string]any{"fingerprint": "dns:故障", "text": "Private alert", "service_name": "DNS"}, false)
	if status != 202 {
		t.Fatalf("accept: %d %s", status, raw)
	}
	var result struct {
		NotificationID    string `json:"notification_id"`
		ServiceID         int64  `json:"service_id"`
		Status            string `json:"status"`
		SubscriptionCount int    `json:"subscription_count"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.NotificationID == "" || result.ServiceID == 0 || result.Status != "no_subscribers" || result.SubscriptionCount != 0 {
		t.Fatalf("receipt: %s", raw)
	}
	status, raw = serviceCall(t, h, "GET", "/api/v1/services/notifications/"+result.NotificationID, "service-secret", "", nil, false)
	if status != 200 || bytes.Contains(raw, []byte("Private alert")) {
		t.Fatalf("query: %d %s", status, raw)
	}
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services/"+itoa(result.ServiceID), "", "", nil, true)
	if status != 200 || !bytes.Contains(raw, []byte("Private alert")) || !bytes.Contains(raw, []byte("dns:故障")) {
		t.Fatalf("admin detail: %d %s", status, raw)
	}
}

func TestE2E_ServiceNotificationRejectsInvalidUnicode(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id, err := h.store.CreateGatewayWorkload("Unicode", "initial", auth.HashAPIKey("unicode-secret"))
	if err != nil {
		t.Fatal(err)
	}
	status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": 1, "publish": true}, true)
	if status != 200 {
		t.Fatalf("grant: %d %s", status, raw)
	}
	req, err := http.NewRequest("POST", h.ts.URL+"/api/v1/services/notifications", bytes.NewBufferString(`{"fingerprint":"\ud800","text":"alert"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer unicode-secret")
	req.Header.Set("Idempotency-Key", "unicode-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("unpaired surrogate accepted: %d", resp.StatusCode)
	}
}

func TestE2E_ServiceNotificationContract(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	const endpoint = "/api/v1/services/notifications"
	workload := func(name, credential string) int64 {
		t.Helper()
		id, err := h.store.CreateGatewayWorkload(name, "initial", auth.HashAPIKey(credential))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	grant := func(id, revision int64, publish, query bool) {
		t.Helper()
		status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": revision, "publish": publish, "query": query}, true)
		if status != 200 {
			t.Fatalf("grant: %d %s", status, raw)
		}
	}
	a := workload("Source A", "a-secret")
	b := workload("Source B", "b-secret")
	payload := map[string]any{"fingerprint": "dns", "service_name": "DNS", "text": "private-body"}
	for _, test := range []struct {
		credential string
		want       int
	}{{"", 401}, {"unknown", 401}, {"a-secret", 403}} {
		status, raw := serviceCall(t, h, "POST", endpoint, test.credential, "first", payload, false)
		if status != test.want {
			t.Fatalf("auth: %d %s", status, raw)
		}
	}
	grant(a, 1, true, true)
	grant(b, 1, true, true)
	status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(a)+"/service-permissions", "", "", map[string]any{"expected_revision": 1, "publish": false}, true)
	if status != 409 {
		t.Fatalf("stale grant: %d %s", status, raw)
	}
	invalid := []map[string]any{
		{"fingerprint": "", "text": "ok"}, {"fingerprint": " x", "text": "ok"}, {"fingerprint": "x\n", "text": "ok"}, {"fingerprint": "x\u0000y", "text": "ok"},
		{"fingerprint": strings.Repeat("界", 513), "text": "ok"}, {"fingerprint": "x", "text": ""}, {"fingerprint": "x", "text": " \n"}, {"fingerprint": "x", "text": strings.Repeat("界", 1366)},
		{"fingerprint": "x", "text": "ok", "service_name": ""}, {"fingerprint": "x", "text": "ok", "service_name": nil}, {"fingerprint": "x", "text": "ok", "service_name": "x\u2028y"},
		{"fingerprint": "x", "text": "ok", "service_name": strings.Repeat("界", 257)}, {"fingerprint": "x", "text": "ok", "parse_mode": "XML"}, {"fingerprint": "x", "text": "ok", "parse_mode": nil},
		{"fingerprint": "x", "text": "ok", "bot_id": 1}, {"fingerprint": "x", "text": "ok", "chat_id": 42}, {"fingerprint": "x", "text": "ok", "subscriptions": []int{1}},
		{"fingerprint": "x", "text": "ok", "route_key": "alerts"}, {"fingerprint": 17, "text": "ok"},
	}
	for i, p := range invalid {
		status, raw := serviceCall(t, h, "POST", endpoint, "a-secret", fmt.Sprintf("invalid-%d", i), p, false)
		if status != 400 {
			t.Fatalf("invalid %d: %d %s", i, status, raw)
		}
	}
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services", "", "", nil, true)
	if status != 200 || string(bytes.TrimSpace(raw)) != "[]" {
		t.Fatalf("rejected input registered services: %d %s", status, raw)
	}
	accept := func(credential, key string, p any) map[string]any {
		t.Helper()
		status, raw := serviceCall(t, h, "POST", endpoint, credential, key, p, false)
		if status != 202 {
			t.Fatalf("accept: %d %s", status, raw)
		}
		if bytes.Contains(raw, []byte("private-body")) {
			t.Fatal("receipt leaks body")
		}
		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := accept("a-secret", "first", payload)
	replay := accept("a-secret", "first", map[string]any{"text": "private-body", "parse_mode": "", "service_name": "DNS", "fingerprint": "dns"})
	if replay["notification_id"] != first["notification_id"] {
		t.Fatal("replay changed identity")
	}
	conflict := map[string]any{"fingerprint": "must-not-register", "text": "private-body"}
	status, raw = serviceCall(t, h, "POST", endpoint, "a-secret", "first", conflict, false)
	if status != 409 {
		t.Fatalf("conflict: %d %s", status, raw)
	}
	renamed := accept("a-secret", "rename", map[string]any{"fingerprint": "dns", "service_name": "New DNS", "text": "private-body"})
	if renamed["service_id"] != first["service_id"] {
		t.Fatal("rename changed service")
	}
	accept("a-secret", "first", payload)
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services/"+fmt.Sprint(first["service_id"]), "", "", nil, true)
	if status != 200 || !bytes.Contains(raw, []byte(`"display_name":"New DNS"`)) {
		t.Fatalf("replay renamed service: %d %s", status, raw)
	}
	other := accept("b-secret", "first", payload)
	sameName := accept("a-secret", "second-fingerprint", map[string]any{"fingerprint": "dns-other", "service_name": "DNS", "text": "private-body"})
	if other["service_id"] == first["service_id"] || sameName["service_id"] == first["service_id"] {
		t.Fatal("service identities merged")
	}
	status, raw = serviceCall(t, h, "GET", endpoint+"/"+first["notification_id"].(string), "b-secret", "", nil, false)
	if status != 404 {
		t.Fatalf("cross-source query: %d %s", status, raw)
	}
	status, missing := serviceCall(t, h, "GET", endpoint+"/missing", "b-secret", "", nil, false)
	if status != 404 || !bytes.Equal(raw, missing) {
		t.Fatal("existence leaked")
	}
	// All character limits are independent of the UTF-8 byte limit for text.
	accept("a-secret", "limits", map[string]any{"fingerprint": strings.Repeat("界", 512), "service_name": strings.Repeat("界", 256), "text": strings.Repeat("界", 1365) + "x", "parse_mode": "MarkdownV2"})
	fallback := accept("a-secret", "fallback", map[string]any{"fingerprint": "fallback", "text": "ok"})
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services/"+fmt.Sprint(fallback["service_id"]), "", "", nil, true)
	if status != 200 || !bytes.Contains(raw, []byte(`"display_name":"fallback"`)) {
		t.Fatalf("fallback: %d %s", status, raw)
	}
	// Independent publish/query grants and rotation share the workload revision.
	grant(b, 2, false, true)
	status, _ = serviceCall(t, h, "POST", endpoint, "b-secret", "new", payload, false)
	if status != 403 {
		t.Fatal("query grant allowed publication")
	}
	grant(b, 3, true, false)
	status, _ = serviceCall(t, h, "GET", endpoint+"/"+other["notification_id"].(string), "b-secret", "", nil, false)
	if status != 403 {
		t.Fatal("publish grant allowed query")
	}
	status, raw = serviceCall(t, h, "POST", "/api/gateway/v1/workloads/"+itoa(a)+"/credentials/rotate", "", "", map[string]any{"expected_revision": 2, "name": "replacement"}, true)
	if status != 200 {
		t.Fatalf("rotation: %d %s", status, raw)
	}
	var rotated struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(raw, &rotated); err != nil {
		t.Fatal(err)
	}
	if accept(rotated.Credential, "first", payload)["notification_id"] != first["notification_id"] {
		t.Fatal("rotation lost idempotency")
	}
	status, _ = serviceCall(t, h, "GET", endpoint+"/"+first["notification_id"].(string), "a-secret", "", nil, false)
	if status != 401 {
		t.Fatal("old credential survived")
	}
	status, raw = serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(a), "", "", map[string]any{"expected_revision": 3, "status": "disabled"}, true)
	if status != 200 {
		t.Fatalf("disable: %d %s", status, raw)
	}
	status, _ = serviceCall(t, h, "POST", endpoint, rotated.Credential, "disabled", payload, false)
	if status != 401 {
		t.Fatal("disabled workload accepted")
	}
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/audit", "", "", nil, true)
	if status != 200 || !bytes.Contains(raw, []byte("service_permissions.replace")) || bytes.Contains(raw, []byte("private-body")) || bytes.Contains(raw, []byte(rotated.Credential)) {
		t.Fatalf("audit: %d %s", status, raw)
	}
	if len(h.fake.Requests()) != 0 {
		t.Fatal("no-subscriber path contacted Telegram")
	}
}

func TestE2E_ServiceNotificationConcurrentRegistration(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id, err := h.store.CreateGatewayWorkload("Concurrent", "initial", auth.HashAPIKey("concurrent-secret"))
	if err != nil {
		t.Fatal(err)
	}
	status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": 1, "publish": true, "query": true}, true)
	if status != 200 {
		t.Fatalf("grant: %d %s", status, raw)
	}
	var wg sync.WaitGroup
	results := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, raw := serviceCall(t, h, "POST", "/api/v1/services/notifications", "concurrent-secret", "same-key", map[string]any{"fingerprint": "parallel", "text": "ok"}, false)
			if status != 202 {
				t.Errorf("concurrent: %d %s", status, raw)
				return
			}
			results <- string(raw)
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for result := range results {
		if first == "" {
			first = result
		}
		if result != first {
			t.Fatal("concurrent replay differs")
		}
	}
	// Distinct first notifications for a second service also share one identity.
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, raw := serviceCall(t, h, "POST", "/api/v1/services/notifications", "concurrent-secret", fmt.Sprintf("distinct-%d", i), map[string]any{"fingerprint": "parallel-distinct", "text": "ok"}, false)
			if status != 202 {
				t.Errorf("distinct: %d %s", status, raw)
			}
		}(i)
	}
	wg.Wait()
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services", "", "", nil, true)
	var services []map[string]any
	if err := json.Unmarshal(raw, &services); err != nil {
		t.Fatal(err)
	}
	if status != 200 || len(services) != 2 {
		t.Fatalf("concurrent services: %d %s", status, raw)
	}
}

func TestE2E_ServiceNotificationRestartAndRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	start := func(st *store.Store) *e2eHarness {
		srv := server.NewServer(st, nil)
		ts := httptest.NewServer(srv.BuildMux())
		return &e2eHarness{store: st, server: srv, ts: ts, session: createTestAuth(t, st)}
	}
	h := start(st)
	id, err := st.CreateGatewayWorkload("Persistent", "initial", auth.HashAPIKey("persist-secret"))
	if err != nil {
		t.Fatal(err)
	}
	status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": 1, "publish": true, "query": true}, true)
	if status != 200 {
		t.Fatalf("grant: %d %s", status, raw)
	}
	payload := map[string]any{"fingerprint": "persistent", "text": "restricted body"}
	status, original := serviceCall(t, h, "POST", "/api/v1/services/notifications", "persist-secret", "persist-1", payload, false)
	if status != 202 {
		t.Fatalf("accept: %d %s", status, original)
	}
	var receipt struct {
		ID        string `json:"notification_id"`
		ServiceID int64  `json:"service_id"`
	}
	if err := json.Unmarshal(original, &receipt); err != nil {
		t.Fatal(err)
	}
	h.ts.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h = start(st)
	defer h.ts.Close()
	status, raw = serviceCall(t, h, "GET", "/api/v1/services/notifications/"+receipt.ID, "persist-secret", "", nil, false)
	if status != 200 || !bytes.Equal(raw, original) {
		t.Fatalf("restart query: %d %s", status, raw)
	}
	status, raw = serviceCall(t, h, "POST", "/api/v1/services/notifications", "persist-secret", "persist-1", payload, false)
	if status != 202 || !bytes.Equal(raw, original) {
		t.Fatalf("restart replay: %d %s", status, raw)
	}
	for _, role := range []string{"operator", "user"} {
		uid, err := st.CreateUser(role, "unused-password-hash", role, role)
		if err != nil {
			t.Fatal(err)
		}
		session := "session-" + role
		if err := st.CreateSession(session, uid, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		adminSession := h.session
		h.session = session
		status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services/"+itoa(receipt.ServiceID), "", "", nil, true)
		if role == "operator" {
			if status != 200 || bytes.Contains(raw, []byte("restricted body")) {
				t.Fatalf("operator detail: %d %s", status, raw)
			}
		} else if status != 403 {
			t.Fatalf("user detail: %d %s", status, raw)
		}
		status, _ = serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": 2, "publish": true}, true)
		if status != 403 {
			t.Fatal("nonadmin changed grants")
		}
		h.session = adminSession
	}
	status, raw = serviceCall(t, h, "GET", "/api/gateway/v1/services/"+itoa(receipt.ServiceID), "", "", nil, true)
	if status != 200 || !bytes.Contains(raw, []byte("restricted body")) {
		t.Fatalf("persisted admin body: %d %s", status, raw)
	}
	// Admin login is never sufficient producer authentication.
	status, _ = serviceCall(t, h, "POST", "/api/v1/services/notifications", "", "admin-attempt", payload, true)
	if status != 401 {
		t.Fatal("admin session authorized producer")
	}
	// A receipt must not report success after SQLite becomes unavailable.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	status, raw = serviceCall(t, h, "POST", "/api/v1/services/notifications", "persist-secret", "unavailable", payload, false)
	if status != 503 || bytes.Contains(raw, []byte("restricted body")) {
		t.Fatalf("storage failure: %d %s", status, raw)
	}
}

func TestE2E_ServiceNotificationMalformedJSON(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	id, err := h.store.CreateGatewayWorkload("Malformed", "initial", auth.HashAPIKey("malformed-secret"))
	if err != nil {
		t.Fatal(err)
	}
	status, raw := serviceCall(t, h, "PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", "", "", map[string]any{"expected_revision": 1, "publish": true}, true)
	if status != 200 {
		t.Fatalf("grant: %d %s", status, raw)
	}
	cases := []struct {
		body, contentType, key string
		want                   int
	}{
		{`{"fingerprint":"x","text":"ok"}`, "text/plain", "event", 400},
		{`{"fingerprint":"x","text":"ok"}`, "application/json", "", 400},
		{`{"fingerprint":"x","text":"ok"}`, "application/json", "bad key", 400},
		{`{"fingerprint":"x","text":"ok"}`, "application/json", strings.Repeat("k", 129), 400},
		{`{"fingerprint":"x","text":"ok"} {}`, "application/json", "event", 400},
		{`{"fingerprint":"x","fingerprint":"y","text":"ok"}`, "application/json", "event", 400},
		{`{"fingerprint":"x","text":"ok"`, "application/json", "event", 400},
		{`null`, "application/json", "event", 400}, {`[]`, "application/json", "event", 400},
		{"{\"fingerprint\":\"\xff\",\"text\":\"ok\"}", "application/json", "event", 400},
		{`{"fingerprint":"\udc00","text":"ok"}`, "application/json", "event", 400},
		{`{"fingerprint":"x","text":"` + strings.Repeat("a", 65536) + `"}`, "application/json", "event", 400},
		{`{"fingerprint":"\ud83d\ude80","text":"ok"}`, "application/json; charset=utf-8", "valid-unicode", 202},
	}
	for i, tc := range cases {
		req, err := http.NewRequest("POST", h.ts.URL+"/api/v1/services/notifications", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", tc.contentType)
		req.Header.Set("Authorization", "Bearer malformed-secret")
		req.Header.Set("Idempotency-Key", tc.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("case %d: %d want %d", i, resp.StatusCode, tc.want)
		}
	}
}
