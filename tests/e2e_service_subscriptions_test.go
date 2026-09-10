package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

func TestE2E_ServiceSubscriptionSends(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	const token = "940001:subscription-secret"
	h.fake.RegisterBot(token, "subscription_bot", 940001)
	h.fake.RegisterChat(token, -100940001, "Operations")
	admin := func(method, path string, body any, want int) []byte {
		t.Helper()
		status, raw := serviceCall(t, h, method, path, "", "", body, true)
		if status != want {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		return raw
	}
	var account models.BotAccount
	json.Unmarshal(admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Subscriber", "token": token}, 201), &account)
	var destination models.TelegramDestination
	json.Unmarshal(admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": "Operations", "bot_account_id": account.ID, "chat_id": -100940001}, 201), &destination)
	workload, err := h.store.CreateGatewayWorkload("Subscriber", "initial", auth.HashAPIKey("subscriber-secret"))
	if err != nil {
		t.Fatal(err)
	}
	admin("PUT", "/api/gateway/v1/workloads/"+itoa(workload)+"/service-permissions", map[string]any{"expected_revision": 1, "publish": true, "query": true}, 200)
	publish := func(key string) models.ServiceNotification {
		t.Helper()
		status, raw := serviceCall(t, h, "POST", "/api/v1/services/notifications", "subscriber-secret", key, map[string]any{"fingerprint": "dns", "text": "<b>DNS failed</b>", "parse_mode": "HTML"}, false)
		if status != 202 {
			t.Fatalf("publish: %d %s", status, raw)
		}
		var n models.ServiceNotification
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	empty := publish("no-subscribers")
	admin("POST", "/api/gateway/v1/services/"+itoa(empty.ServiceID)+"/subscriptions", map[string]any{"expected_revision": 1, "destination_id": destination.ID}, 201)
	if replay := publish("no-subscribers"); replay.ID != empty.ID || replay.Status != "no_subscribers" {
		t.Fatalf("empty replay: %+v", replay)
	}
	n := publish("subscribed")
	if n.Status != "accepted" || n.SubscriptionCount != 1 || len(n.Deliveries) != 1 {
		t.Fatalf("plan: %+v", n)
	}
	queue := newMemoryOutboundQueue()
	worker := gateway.NewService(h.store, queue, gateway.NewTelegramHTTPClient(h.fake.URL()), "subscriptions")
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	h.Eventually(func() bool {
		status, raw := serviceCall(t, h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		var result models.ServiceNotification
		json.Unmarshal(raw, &result)
		return status == 200 && result.DeliverySummary.Status == "succeeded" && result.DeliverySummary.Succeeded == 1
	}, 5*time.Second, "service notification delivered")
	requests := h.fake.RequestsFor("sendMessage")
	if len(requests) != 1 {
		t.Fatalf("sends: %d", len(requests))
	}
	var sent struct {
		ChatID    int64  `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if err := json.Unmarshal(requests[0].body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ChatID != -100940001 || sent.Text != "<b>DNS failed</b>" || sent.ParseMode != "HTML" {
		t.Fatalf("sent: %+v", sent)
	}
	if replay := publish("subscribed"); replay.ID != n.ID || replay.Deliveries[0].ID != n.Deliveries[0].ID {
		t.Fatal("replay changed plan")
	}
}

type subscriptionFixture struct {
	t           *testing.T
	h           *e2eHarness
	account     models.BotAccount
	destination models.TelegramDestination
	serviceID   int64
}

func setupSubscription(t *testing.T, opts ...e2eOpt) *subscriptionFixture {
	t.Helper()
	f := &subscriptionFixture{t: t, h: setupE2E(t, append(opts, withHTTPServer())...)}
	f.h.fake.RegisterBot("940001:subscription-secret", "subscriber", 940001)
	f.h.fake.RegisterChat("940001:subscription-secret", -100940001, "A")
	f.h.fake.RegisterChat("940001:subscription-secret", -100940002, "B")
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Subscriber", "token": "940001:subscription-secret"}, 201), &f.account)
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": "A", "bot_account_id": f.account.ID, "chat_id": -100940001}, 201), &f.destination)
	id, err := f.h.store.CreateGatewayWorkload("Subscriptions", "initial", auth.HashAPIKey("subscriber-secret"))
	if err != nil {
		t.Fatal(err)
	}
	f.admin("PUT", "/api/gateway/v1/workloads/"+itoa(id)+"/service-permissions", map[string]any{"expected_revision": 1, "publish": true, "query": true}, 200)
	n := f.publish("register", "DNS")
	f.serviceID = n.ServiceID
	return f
}
func (f *subscriptionFixture) admin(method, path string, body any, want int) []byte {
	f.t.Helper()
	status, raw := serviceCall(f.t, f.h, method, path, "", "", body, true)
	if status != want {
		f.t.Fatalf("%s %s: %d %s want %d", method, path, status, raw, want)
	}
	return raw
}
func (f *subscriptionFixture) publish(key, text string) models.ServiceNotification {
	f.t.Helper()
	status, raw := serviceCall(f.t, f.h, "POST", "/api/v1/services/notifications", "subscriber-secret", key, map[string]any{"fingerprint": "dns", "text": text}, false)
	if status != 202 {
		f.t.Fatalf("publish: %d %s", status, raw)
	}
	var n models.ServiceNotification
	if err := json.Unmarshal(raw, &n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *subscriptionFixture) subscribe(destination, revision int64, want int) int64 {
	f.t.Helper()
	raw := f.admin("POST", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions", map[string]any{"destination_id": destination, "expected_revision": revision}, want)
	var result struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(raw, &result)
	return result.ID
}
func (f *subscriptionFixture) await(n models.ServiceNotification, status string) models.ServiceNotification {
	f.t.Helper()
	var result models.ServiceNotification
	f.h.Eventually(func() bool {
		code, raw := serviceCall(f.t, f.h, "GET", "/api/v1/services/notifications/"+n.ID, "subscriber-secret", "", nil, false)
		json.Unmarshal(raw, &result)
		return code == 200 && result.DeliverySummary.Status == status
	}, 5*time.Second, "notification "+status)
	return result
}

func TestE2E_ServiceSubscriptionManagementConflicts(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	f.subscribe(f.destination.ID, 1, 409)
	f.subscribe(f.destination.ID, 2, 409)
	// A second logical account for the same actual Bot cannot duplicate delivery.
	var alias models.BotAccount
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Alias", "token": "940001:subscription-secret"}, 201), &alias)
	var aliasDestination models.TelegramDestination
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": "Alias A", "bot_account_id": alias.ID, "chat_id": -100940001}, 201), &aliasDestination)
	f.subscribe(aliasDestination.ID, 2, 409)
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Another alias", "token": "940001:subscription-secret"}, 201), &alias)
	var b models.TelegramDestination
	json.Unmarshal(f.admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": "B", "bot_account_id": alias.ID, "chat_id": -100940002}, 201), &b)
	f.subscribe(b.ID, 2, 201)
	// Destination updates must also preserve physical subscription uniqueness.
	f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(b.ID), map[string]any{"name": "Collision", "bot_account_id": alias.ID, "chat_id": -100940001, "expected_revision": 1}, 409)
	// Omitting a version cannot silently overwrite a newer destination.
	f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(b.ID), map[string]any{"name": "B", "bot_account_id": alias.ID, "chat_id": -100940002}, 400)
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(alias.ID), map[string]any{"name": "Alias"}, 400)
	f.admin("PUT", "/api/telegram-destinations/"+itoa(b.ID), map[string]any{"name": "B", "bot_account_id": alias.ID, "chat_id": -100940002}, 400)
	f.admin("PUT", "/api/bot-accounts/"+itoa(alias.ID), map[string]any{"name": "Alias"}, 400)
	adminSession := f.h.session
	for _, role := range []string{"operator", "user"} {
		id, err := f.h.store.CreateUser(role, "unused", role, role)
		if err != nil {
			t.Fatal(err)
		}
		session := "subscription-" + role
		if err := f.h.store.CreateSession(session, id, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		f.h.session = session
		f.subscribe(f.destination.ID, 3, 403)
		f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(b.ID), map[string]any{"name": "B", "bot_account_id": alias.ID, "chat_id": -100940002, "expected_revision": 1}, 403)
	}
	f.h.session = adminSession
	raw := f.admin("GET", "/api/gateway/v1/audit", nil, 200)
	if !bytes.Contains(raw, []byte("subscription.create")) || bytes.Contains(raw, []byte("subscription-secret")) {
		t.Fatalf("audit: %s", raw)
	}
}

func TestE2E_ServiceSubscriptionSnapshotsAndRotation(t *testing.T) {
	f := setupSubscription(t)
	subscription := f.subscribe(f.destination.ID, 1, 201)
	failure := f.publish("failure", "DNS failed")
	f.admin("PUT", "/api/gateway/v1/destinations/"+itoa(f.destination.ID), map[string]any{"name": "B", "bot_account_id": f.account.ID, "chat_id": -100940002, "expected_revision": 1}, 200)
	recovery := f.publish("recovery", "DNS recovered")
	f.admin("DELETE", "/api/gateway/v1/services/"+itoa(f.serviceID)+"/subscriptions/"+itoa(subscription), map[string]any{"expected_revision": 2}, 200)
	if n := f.publish("after-cancel", "New event"); n.Status != "no_subscribers" {
		t.Fatalf("cancel: %+v", n)
	}
	const rotated = "940001:rotated-secret"
	f.h.fake.RegisterBot(rotated, "subscriber", 940001)
	f.h.fake.RegisterChat(rotated, -100940001, "A")
	f.h.fake.RegisterChat(rotated, -100940002, "B")
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID), map[string]any{"name": "Subscriber", "token": rotated, "expected_revision": 1}, 200)
	worker := gateway.NewService(f.h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(f.h.fake.URL()), "snapshots")
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	f.await(failure, "succeeded")
	f.await(recovery, "succeeded")
	requests := f.h.fake.RequestsFor("sendMessage")
	if len(requests) != 2 {
		t.Fatalf("sends: %d", len(requests))
	}
	targets := map[string]int64{}
	for _, request := range requests {
		var sent struct {
			Text   string `json:"text"`
			ChatID int64  `json:"chat_id"`
		}
		json.Unmarshal(request.body, &sent)
		targets[sent.Text] = sent.ChatID
		if request.token != rotated {
			t.Fatal("old delivery did not use rotated token")
		}
	}
	if targets["DNS failed"] != -100940001 || targets["DNS recovered"] != -100940002 {
		t.Fatalf("changed accepted recipients: %+v", targets)
	}
	if n := f.publish("failure", "DNS failed"); n.ID != failure.ID || n.Deliveries[0].ID != failure.Deliveries[0].ID {
		t.Fatal("replay changed cancelled subscription snapshot")
	}
}

func TestE2E_ServiceSubscriptionBotReplacementFails(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	n := f.publish("old-bot", "Original Bot only")
	f.h.fake.RegisterBot("940009:replacement-secret", "replacement", 940009)
	f.h.fake.RegisterChat("940009:replacement-secret", -100940001, "A")
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID), map[string]any{"name": "Replacement", "token": "940009:replacement-secret", "expected_revision": 1}, 200)
	queue := newMemoryOutboundQueue()
	worker := gateway.NewService(f.h.store, queue, gateway.NewTelegramHTTPClient(f.h.fake.URL()), "replacement")
	operations := gateway.NewOperations(f.h.store, nil, nil, worker, nil, nil, nil)
	f.h.server.SetGatewayOperations(operations)
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	result := f.await(n, "failed")
	if result.Deliveries[0].Status != "dead-lettered" || f.h.fake.RequestsCountFor("sendMessage") != 0 {
		t.Fatalf("replacement misdelivered: %+v", result)
	}
	raw := f.admin("GET", "/api/gateway/v1/ops/dlq", nil, 200)
	if !bytes.Contains(raw, []byte(n.Deliveries[0].ID)) || !bytes.Contains(raw, []byte("service_bot_identity_changed")) {
		t.Fatalf("service failure missing from DLQ: %s", raw)
	}
}

func TestE2E_ServiceSubscriptionBrowser(t *testing.T) {
	node := os.Getenv("BROWSER_NODE")
	if node == "" {
		t.Skip("set BROWSER_NODE and PUPPETEER_MODULE for browser acceptance")
	}
	f := setupSubscription(t)
	uid, err := f.h.store.CreateUser("browser-admin", "unused", "Browser administrator", "admin")
	if err != nil {
		t.Fatal(err)
	}
	f.h.session = "browser-subscriptions"
	if err := f.h.store.CreateSession(f.h.session, uid, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var once sync.Once
	releaseSend := func() { once.Do(func() { close(release) }) }
	f.h.fake.SetHandler("release", func(w http.ResponseWriter, r *http.Request) { releaseSend(); fmt.Fprint(w, `{"ok":true}`) })
	f.h.fake.SetHandler("sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Text == "<b>Browser DNS</b>" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if body.Text == "Browser failure" {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"ok":false,"error_code":403}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":994}}`)
	})
	f.h.fake.RegisterBot("940003:browser-secret", "browser_bot", 940003)
	f.h.fake.RegisterChat("940003:browser-secret", -100940003, "New browser group")
	worker := gateway.NewService(f.h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(f.h.fake.URL()), "browser")
	worker.Start(context.Background())
	t.Cleanup(worker.Stop)
	t.Cleanup(releaseSend)
	script, err := filepath.Abs("browser_service_subscriptions.cjs")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	cmd.Env = append(os.Environ(), "SUBSCRIPTION_URL="+f.h.ts.URL, "SUBSCRIPTION_TELEGRAM="+f.h.fake.URL(), "SUBSCRIPTION_COOKIE_NAME="+auth.SessionCookieName, "SUBSCRIPTION_SESSION="+f.h.session, "SUBSCRIPTION_ACCOUNT="+itoa(f.account.ID), "SUBSCRIPTION_DESTINATION="+itoa(f.destination.ID))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	}
	t.Log(string(output))
}
