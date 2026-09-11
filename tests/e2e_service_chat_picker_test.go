package tests

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
)

func setupServiceChatPicker(t *testing.T) *subscriptionFixture {
	t.Helper()
	f := setupSubscription(t)
	botID, err := f.h.store.AddBotConfig(models.BotConfig{Token: "940001:subscription-secret", Name: "Subscriber", BotUsername: "subscriber"})
	if err != nil {
		t.Fatal(err)
	}
	for _, chat := range []models.Chat{{ID: -100940001, Type: "private", Title: "Existing recipient"}, {ID: -100940002, Type: "group", Title: `Discovered "group" <img src=x onerror=alert(1)>`}} {
		if err := f.h.store.UpsertChat(botID, chat); err != nil {
			t.Fatal(err)
		}
	}
	other, err := f.h.store.AddBotConfig(models.BotConfig{Token: "940099:other-secret", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.store.UpsertChat(other, models.Chat{ID: -100940099, Type: "group", Title: "Other Bot private group"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestE2E_ServiceChatPickerListsKnownChatsWithoutRegisteringTargets(t *testing.T) {
	f := setupServiceChatPicker(t)
	raw := f.admin("GET", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID)+"/chats", nil, 200)
	var chats []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(raw, &chats); err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("want both known chats, got %s", raw)
	}
	found := map[int64]string{}
	for _, chat := range chats {
		found[chat.ID] = chat.Title
	}
	if found[-100940001] != "Existing recipient" || found[-100940002] != `Discovered "group" <img src=x onerror=alert(1)>` {
		t.Fatalf("wrong Bot chats: %s", raw)
	}
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "last_msg") {
		t.Fatalf("chat choices leaked unrelated fields: %s", raw)
	}
	var targets []models.TelegramDestination
	json.Unmarshal(f.admin("GET", "/api/gateway/v1/destinations", nil, 200), &targets)
	if len(targets) != 1 {
		t.Fatal("viewing chats registered a destination")
	}
	f.admin("GET", "/api/gateway/v1/bot-accounts/999999/chats", nil, 404)
	status, _ := serviceCall(t, f.h, "GET", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID)+"/chats", "subscriber-secret", "", nil, false)
	if status != 401 && status != 403 {
		t.Fatalf("producer could list chats: %d", status)
	}
}

func TestE2E_ServiceChatPickerBrowser(t *testing.T) {
	node := os.Getenv("BROWSER_NODE")
	if node == "" {
		t.Skip("set BROWSER_NODE and PUPPETEER_MODULE for browser acceptance")
	}
	f := setupServiceChatPicker(t)
	uid, err := f.h.store.CreateUser("browser-chat-admin", "unused", "Browser chat administrator", "admin")
	if err != nil {
		t.Fatal(err)
	}
	f.h.session = "browser-chat-picker"
	if err := f.h.store.CreateSession(f.h.session, uid, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("browser_service_chat_picker.cjs")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	cmd.Env = append(os.Environ(), "SUBSCRIPTION_URL="+f.h.ts.URL, "SUBSCRIPTION_COOKIE_NAME="+auth.SessionCookieName, "SUBSCRIPTION_SESSION="+f.h.session, "SUBSCRIPTION_ACCOUNT="+itoa(f.account.ID))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
