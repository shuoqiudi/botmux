package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/configbackup"
	"github.com/skrashevich/botmux/internal/models"
)

func TestE2E_ConfigNotificationScriptRoundTrip(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	// Same Bot/multiple chats and same chat/different Bots must stay distinct.
	var b models.TelegramDestination
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": "B", "bot_account_id": f.account.ID, "chat_id": -100940002}, 201), &b)
	f.subscribe(b.ID, 2, 201)
	secondID, err := f.h.store.AddBotAccount(models.BotAccount{Name: "Second Bot", Token: "940002:second-secret"})
	if err != nil {
		t.Fatal(err)
	}
	cID, err := f.h.store.AddTelegramDestination(models.TelegramDestination{Name: "C", BotAccountID: secondID, ChatID: -100940001, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	f.subscribe(cID, 3, 201)
	cancelledID, err := f.h.store.AddTelegramDestination(models.TelegramDestination{Name: "Cancelled", BotAccountID: secondID, ChatID: -100940003, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := f.subscribe(cancelledID, 4, 201)
	f.admin("DELETE", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions/"+itoa(cancelled), map[string]any{"expected_revision": 5}, 200)
	disabledID, err := f.h.store.AddTelegramDestination(models.TelegramDestination{Name: "Disabled", BotAccountID: secondID, ChatID: -100940004, Status: "disabled"})
	if err != nil || disabledID == 0 {
		t.Fatal(err)
	}
	old := f.publish("old", "before restore")

	target := setupE2E(t, withHTTPServer())
	startConfigGateway(t, target)
	target.fake.RegisterBot("940001:subscription-secret", "subscriber", 940001)
	target.fake.RegisterChat("940001:subscription-secret", -100940001, "A")
	target.fake.RegisterChat("940001:subscription-secret", -100940002, "B")
	target.fake.RegisterBot("940002:second-secret", "second", 940002)
	target.fake.RegisterChat("940002:second-secret", -100940001, "C")
	file := filepath.Join(t.TempDir(), "notifications.json")
	if out, err := configScript(t, f.h, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("subscriber-secret")) {
		t.Fatal("plaintext workload credential exported")
	}
	for i := 0; i < 2; i++ {
		if out, err := configScript(t, target, "restore", file); err != nil {
			t.Fatalf("restore: %v %s", err, out)
		}
	}
	// Exact re-export preserves inactive relationships and disabled destinations.
	roundTrip := filepath.Join(t.TempDir(), "roundtrip.json")
	if out, err := configScript(t, target, "export", roundTrip); err != nil {
		t.Fatalf("reexport: %v %s", err, out)
	}
	restored, err := os.ReadFile(roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, restored) {
		t.Fatal("subscription or destination state changed")
	}
	services, err := target.store.ListNotificationServices(context.Background())
	if err != nil || len(services) != 1 {
		t.Fatalf("services: %v %v", services, err)
	}
	if services[0].LastReceivedAt != "" {
		t.Fatal("activity timestamp restored")
	}
	status, _ := serviceCall(t, target, "GET", "/api/v1/services/notifications/"+old.ID, "subscriber-secret", "", nil, false)
	if status != 404 {
		t.Fatalf("old history restored: %d", status)
	}
	if len(target.fake.RequestsFor("sendMessage")) != 0 {
		t.Fatal("restore sent history")
	}
	status, body := serviceCall(t, target, "POST", "/api/v1/services/notifications", "subscriber-secret", "old", map[string]any{"fingerprint": "dns", "service_name": "Updated DNS", "text": "after restore"}, false)
	if status != 202 {
		t.Fatalf("publish with original credential: %d %s", status, body)
	}
	var n models.ServiceNotification
	if err := json.Unmarshal(body, &n); err != nil {
		t.Fatal(err)
	}
	if n.SubscriptionCount != 3 || n.ID == old.ID {
		t.Fatalf("new notification: %+v", n)
	}
	target.Eventually(func() bool { return len(target.fake.RequestsFor("sendMessage")) == 3 }, 5*time.Second, "restored delivery")
	seen := map[string]bool{}
	for _, r := range target.fake.RequestsFor("sendMessage") {
		var sent struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(r.body, &sent); err != nil {
			t.Fatal(err)
		}
		if sent.Text != "after restore" {
			t.Fatal("wrong notification text")
		}
		seen[r.token+":"+itoa(sent.ChatID)] = true
	}
	for _, key := range []string{"940001:subscription-secret:-100940001", "940001:subscription-secret:-100940002", "940002:second-secret:-100940001"} {
		if !seen[key] {
			t.Fatalf("missing destination %s", key)
		}
	}
	services, err = target.store.ListNotificationServices(context.Background())
	if err != nil || services[0].DisplayName != "Updated DNS" {
		t.Fatal("name update lost")
	}
	// A page-visible name change invalidates the old restore receipt.
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	if status != 409 {
		t.Fatalf("changed service retry: %d", status)
	}
}

func TestE2E_ConfigNotificationSourcesAndCredentials(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	target := setupE2E(t, withHTTPServer())
	startConfigGateway(t, target)
	create := func(name, credential string, publish, query bool) *models.GatewayWorkloadAdmin {
		t.Helper()
		w, err := source.store.CreateGatewayWorkloadAdmin(name, "initial", auth.HashAPIKey(credential), "test")
		if err != nil {
			t.Fatal(err)
		}
		w, err = source.store.SetServicePermissions(w.ID, w.Revision, models.ServicePermissions{Publish: publish, Query: query}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	first := create("First", "original", true, true)
	second := create("Second", "second", true, false)
	create("Read only", "reader", false, true)
	for _, credential := range []string{"original", "second"} {
		status, raw := serviceCall(t, source, "POST", "/api/v1/services/notifications", credential, "same-key", map[string]any{"fingerprint": "v2:incident:domains:dns", "service_name": "DNS for " + credential, "text": "old"}, false)
		if status != 202 {
			t.Fatalf("register: %d %s", status, raw)
		}
	}
	rotated, err := source.store.RotateGatewayWorkloadCredential(first.ID, first.Revision, "replacement", auth.HashAPIKey("replacement"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rotated.Credentials) != 2 {
		t.Fatal("rotation fixture")
	}
	if _, err := source.store.SetGatewayWorkloadStatus(second.ID, second.Revision, "disabled", "test"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "sources.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot configbackup.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workloads) != 3 || len(snapshot.NotificationServices) != 2 {
		t.Fatal("missing sources or unsubscribed services")
	}
	// Pre-existing target sequences demonstrate that local IDs are rebuilt.
	if _, err := target.store.DB().Exec(`UPDATE sqlite_sequence SET seq=100 WHERE name='gateway_workloads'; INSERT INTO sqlite_sequence(name,seq) VALUES('gateway_notification_services',100)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out, err := configScript(t, target, "restore", file); err != nil {
			t.Fatalf("restore: %v %s", err, out)
		}
	}
	services, err := target.store.ListNotificationServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 || services[0].WorkloadID == services[1].WorkloadID {
		t.Fatal("source isolation lost")
	}
	for _, s := range services {
		if s.ID <= 100 || s.WorkloadID <= 100 || s.LastReceivedAt != "" || s.SubscriptionCount != 0 {
			t.Fatalf("restored service: %+v", s)
		}
	}
	// Source credentials retain their original validity, independently of instance keys.
	for _, tc := range []struct {
		credential string
		want       int
	}{{"original", 401}, {"replacement", 202}, {"second", 401}, {"reader", 403}, {auth.HashAPIKey("replacement"), 401}} {
		status, body := serviceCall(t, target, "POST", "/api/v1/services/notifications", tc.credential, "same-key", map[string]any{"fingerprint": "v2:incident:domains:dns", "service_name": "DNS for original", "text": "new"}, false)
		if status != tc.want {
			t.Fatalf("credential status=%d want=%d: %s", status, tc.want, body)
		}
		if status == 202 {
			var n models.ServiceNotification
			json.Unmarshal(body, &n)
			if n.Status != "no_subscribers" {
				t.Fatal("unsubscribed service delivered")
			}
		}
	}
	for _, endpoint := range []struct{ method, path string }{{"GET", "/api/config/export"}, {"POST", "/api/config/restore"}} {
		status, _ := serviceCall(t, target, endpoint.method, endpoint.path, "replacement", "", json.RawMessage(raw), false)
		if status != 401 {
			t.Fatalf("workload accessed management: %d", status)
		}
	}
	// Runtime reads and new notifications without a name edit do not invalidate receipts.
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry after activity: %v %s", err, out)
	}
	secondFile := filepath.Join(t.TempDir(), "again.json")
	if out, err := configScript(t, target, "export", secondFile); err != nil {
		t.Fatalf("reexport: %v %s", err, out)
	}
	again, err := os.ReadFile(secondFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Fatal("cross-instance snapshot changed")
	}
	workloads, err := target.store.GetGatewayWorkloadsAdmin()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workloads {
		if w.Name == "First" {
			if _, err := target.store.SetServicePermissions(w.ID, w.Revision, models.ServicePermissions{}, "test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	status, _ := serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	if status != 409 {
		t.Fatalf("permission edit did not conflict: %d", status)
	}
}

func TestE2E_ConfigNotificationValidationAndRollback(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	status, raw := serviceCall(t, f.h, "GET", "/api/config/export", "", "", nil, true)
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	target := setupE2E(t, withHTTPServer())
	for _, tc := range []struct {
		name   string
		change func(*configbackup.Snapshot)
		want   int
	}{
		{"missing source", func(s *configbackup.Snapshot) { s.NotificationServices[0].WorkloadRef = "absent" }, 400},
		{"duplicate service", func(s *configbackup.Snapshot) {
			s.NotificationServices = append(s.NotificationServices, s.NotificationServices[0])
		}, 400},
		{"missing service", func(s *configbackup.Snapshot) { s.Subscriptions[0].Fingerprint = "absent" }, 400},
		{"missing destination", func(s *configbackup.Snapshot) { s.Subscriptions[0].DestinationRef = "absent" }, 400},
		{"duplicate recipient", func(s *configbackup.Snapshot) {
			p := s.Subscriptions[0]
			p.Ref = "duplicate"
			s.Subscriptions = append(s.Subscriptions, p)
		}, 400},
		{"duplicate alias recipient", func(s *configbackup.Snapshot) {
			d := s.Destinations[0]
			d.Ref = "alias"
			s.Destinations = append(s.Destinations, d)
			p := s.Subscriptions[0]
			p.Ref = "alias"
			p.DestinationRef = "alias"
			s.Subscriptions = append(s.Subscriptions, p)
		}, 400},
		{"invalid verifier", func(s *configbackup.Snapshot) { s.Workloads[0].Credentials[0].Verifier = "not-a-hash" }, 400},
		{"plaintext algorithm", func(s *configbackup.Snapshot) { s.Workloads[0].Credentials[0].Algorithm = "plaintext" }, 400},
		{"duplicate credential", func(s *configbackup.Snapshot) {
			s.Workloads[0].Credentials = append(s.Workloads[0].Credentials, s.Workloads[0].Credentials[0])
		}, 400},
		{"invalid status", func(s *configbackup.Snapshot) { s.Workloads[0].Status = "enabled" }, 400},
		{"route permission", func(s *configbackup.Snapshot) {
			s.Workloads[0].RoutePermissions = []configbackup.RoutePermission{{RouteKey: "alerts", Action: "messages.send"}}
		}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot configbackup.Snapshot
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			tc.change(&snapshot)
			status, _ := serviceCall(t, target, "POST", "/api/config/restore", "", "", snapshot, true)
			if status != tc.want {
				t.Fatalf("status=%d want=%d", status, tc.want)
			}
			assertEmptyNotificationConfig(t, target)
		})
	}
	// Unknown permissions cannot be dropped by decoding into permissive structs.
	malformed := bytes.Replace(raw, []byte(`"publish": true`), []byte(`"publish": true, "admin": true`), 1)
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(malformed), true)
	if status != 400 {
		t.Fatalf("unknown permission accepted: %d", status)
	}
	// Inject a storage failure at the final receipt write, after all configuration inserts.
	if _, err := target.store.DB().Exec(`CREATE TRIGGER fail_notification_restore BEFORE INSERT ON configuration_restores BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	if status != 503 {
		t.Fatalf("fault injection: %d", status)
	}
	assertEmptyNotificationConfig(t, target)
	if _, err := target.store.DB().Exec(`DROP TRIGGER fail_notification_restore`); err != nil {
		t.Fatal(err)
	}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	if status != 200 {
		t.Fatalf("valid retry: %d", status)
	}
	services, err := target.store.ListNotificationServices(context.Background())
	if err != nil || len(services) != 1 {
		t.Fatal("missing restored service")
	}
	subscriptions, err := target.store.ListServiceSubscriptions(context.Background(), services[0].ID)
	if err != nil || len(subscriptions) != 1 {
		t.Fatal("missing restored subscription")
	}
	if err := target.store.CancelServiceSubscription(context.Background(), services[0].ID, subscriptions[0].ID, services[0].Revision, "test"); err != nil {
		t.Fatal(err)
	}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(raw), true)
	if status != 409 {
		t.Fatalf("cancelled subscription resurrected: %d", status)
	}
}

func assertEmptyNotificationConfig(t *testing.T, h *e2eHarness) {
	t.Helper()
	status, raw := serviceCall(t, h, "GET", "/api/config/export", "", "", nil, true)
	var snapshot configbackup.Snapshot
	if status != 200 {
		t.Fatalf("target export: %d", status)
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.BusinessRoutes)+len(snapshot.Bots)+len(snapshot.Workloads)+len(snapshot.NotificationServices)+len(snapshot.Subscriptions)+len(snapshot.Destinations) != 0 {
		t.Fatal("failed restore left partial configuration")
	}
}
