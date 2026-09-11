package tests

import (
	"bytes"
	"encoding/json"
	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/skrashevich/botmux/internal/models"
)

func TestE2E_ServiceImportIsRepeatableWithoutNotifications(t *testing.T) {
	f := setupSubscription(t)
	var services []models.NotificationService
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services", nil, 200), &services)
	body := map[string]any{"migration_id": "dns-cutover", "services": []any{map[string]any{
		"workload_id": services[0].WorkloadID, "fingerprint": "v2:incident:domains:dns", "display_name": "DNS",
		"destinations": []any{map[string]any{"destination_id": f.destination.ID, "bot_account_id": f.account.ID, "telegram_bot_id": 940001, "chat_id": -100940001}},
	}}}
	first := f.admin("POST", "/api/gateway/v1/services/import", body, 200)
	second := f.admin("POST", "/api/gateway/v1/services/import", body, 200)
	if string(first) != string(second) {
		t.Fatal("repeated import changed receipt")
	}
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services", nil, 200), &services)
	var imported models.NotificationService
	for _, s := range services {
		if s.Fingerprint == "v2:incident:domains:dns" {
			imported = s
		}
	}
	if imported.ID == 0 || imported.SubscriptionCount != 1 || imported.LastReceivedAt != "" {
		t.Fatalf("import: %+v", imported)
	}
	var detail struct {
		Notifications []models.ServiceNotification
		Subscriptions []models.ServiceSubscription
	}
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services/"+itoa(imported.ID), nil, 200), &detail)
	if len(detail.Notifications) != 0 || len(detail.Subscriptions) != 1 || detail.Subscriptions[0].DestinationID != f.destination.ID {
		t.Fatalf("detail: %+v", detail)
	}
	f.admin("DELETE", "/api/gateway/v1/services/"+itoa(imported.ID)+"/subscriptions/"+itoa(detail.Subscriptions[0].ID), map[string]any{"expected_revision": imported.Revision}, 200)
	f.admin("POST", "/api/gateway/v1/services/import", body, 200)
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services/"+itoa(imported.ID), nil, 200), &detail)
	if len(detail.Subscriptions) != 0 {
		t.Fatal("replay resurrected cancelled subscription")
	}
}

func TestE2E_ServiceImportRejectsDriftAtomically(t *testing.T) {
	f := setupSubscription(t)
	var services []models.NotificationService
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services", nil, 200), &services)
	destination := map[string]any{"destination_id": f.destination.ID, "bot_account_id": f.account.ID, "telegram_bot_id": 940001, "chat_id": -100940001}
	bad := map[string]any{"destination_id": f.destination.ID, "bot_account_id": f.account.ID, "telegram_bot_id": 940001, "chat_id": -100999999}
	entry := func(fingerprint string, d map[string]any) any {
		return map[string]any{"workload_id": services[0].WorkloadID, "fingerprint": fingerprint, "display_name": "DNS", "destinations": []any{d}}
	}
	body := map[string]any{"migration_id": "atomic", "services": []any{entry("first", destination), entry("second", bad)}}
	f.admin("POST", "/api/gateway/v1/services/import", body, 409)
	var after []models.NotificationService
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services", nil, 200), &after)
	if len(after) != 1 {
		t.Fatal("failed batch left partial services")
	}
	body["services"] = []any{entry("first", destination), entry("second", destination)}
	f.admin("POST", "/api/gateway/v1/services/import", body, 200)
	body["services"] = []any{entry("first", destination)}
	f.admin("POST", "/api/gateway/v1/services/import", body, 409)
	status, _ := serviceCall(t, f.h, "POST", "/api/gateway/v1/services/import", "subscriber-secret", "", body, false)
	if status != 401 && status != 403 {
		t.Fatalf("workload import authorized: %d", status)
	}
}

func TestE2E_ServiceImportLostResponseSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "import.db")
	st, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f := setupSubscription(t, func(h *e2eHarness) {
		h.store = st
		h.server = server.NewServer(st, nil)
		h.server.TgAPIBaseURL = h.fake.URL()
		h.session = createTestAuth(t, st)
	})
	t.Cleanup(func() { f.h.store.Close() })
	var services []models.NotificationService
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services", nil, 200), &services)
	body := map[string]any{"migration_id": "lost-response", "services": []any{map[string]any{"workload_id": services[0].WorkloadID, "fingerprint": "imported", "display_name": "DNS", "destinations": []any{map[string]any{"destination_id": f.destination.ID, "bot_account_id": f.account.ID, "telegram_bot_id": 940001, "chat_id": -100940001}}}}}
	raw, _ := json.Marshal(body)
	// Run the real admin handler, then lose its committed response at the HTTP boundary.
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		f.h.server.BuildMux().ServeHTTP(recorder, r)
		if recorder.Code != 200 {
			t.Errorf("import before disconnect: %d", recorder.Code)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	req, _ := http.NewRequest("POST", dropped.URL+"/api/gateway/v1/services/import", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.h.session})
	if response, err := http.DefaultClient.Do(req); err == nil {
		response.Body.Close()
		t.Fatal("expected lost response")
	}
	dropped.Close()
	f.h.ts.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f.h.store = st
	f.h.server = server.NewServer(st, nil)
	f.h.ts = httptest.NewServer(f.h.server.BuildMux())
	t.Cleanup(f.h.ts.Close)
	f.admin("POST", "/api/gateway/v1/services/import", body, 200)
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/services", nil, 200), &services)
	if len(services) != 2 {
		t.Fatalf("services after restart: %d", len(services))
	}
	for _, s := range services {
		if s.Fingerprint == "imported" {
			if s.SubscriptionCount != 1 || s.LastReceivedAt != "" {
				t.Fatalf("replayed import: %+v", s)
			}
			var detail struct{ Notifications []models.ServiceNotification }
			json.Unmarshal(f.admin("GET", "/api/gateway/v1/services/"+itoa(s.ID), nil, 200), &detail)
			if len(detail.Notifications) != 0 {
				t.Fatal("import generated a notification")
			}
		}
	}
}
