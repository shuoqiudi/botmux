package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/bridge"
	"github.com/skrashevich/botmux/internal/models"
)

const testYandexWebhookSecret = "test-yandex-webhook-secret"

func yandexUpdateBody(chatID, login, text string, messageID int64) []byte {
	upd := map[string]any{
		"update_id":  1,
		"message_id": messageID,
		"text":       text,
		"timestamp":  time.Now().Unix(),
		"chat": map[string]any{
			"id":   chatID,
			"type": "private",
		},
		"from": map[string]any{
			"login":        login,
			"display_name": "Alice YM",
		},
	}
	b, _ := json.Marshal(upd)
	return b
}

func yandexImageUpdateBody(chatID string, messageID int64, fileID, caption string) []byte {
	upd := map[string]any{
		"update_id":  2,
		"message_id": messageID,
		"text":       caption,
		"timestamp":  time.Now().Unix(),
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"from":       map[string]any{"login": "alice", "display_name": "Alice YM"},
		"image":      map[string]any{"file_id": fileID, "width": 800, "height": 600, "name": "photo.png"},
	}
	b, _ := json.Marshal(upd)
	return b
}

func postYandexBridgeIncoming(t *testing.T, serverURL string, bridgeID int64, body []byte, secret string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/bridge/%d/incoming", serverURL, bridgeID)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("postYandexBridgeIncoming: NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postYandexBridgeIncoming: Do: %v", err)
	}
	return resp
}

func addYandexBridge(t *testing.T, h *e2eHarness, botID int64, apiBaseURL, webhookSecret string) int64 {
	t.Helper()
	cfg := bridge.YandexConfig{
		BotToken:      "test-yandex-token",
		WebhookSecret: webhookSecret,
		APIBaseURL:    apiBaseURL,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("addYandexBridge: marshal config: %v", err)
	}
	bridgeID, err := h.store.AddBridge(models.BridgeConfig{
		Name:        "test-yandex-bridge",
		Protocol:    "yandex",
		LinkedBotID: botID,
		Config:      string(cfgJSON),
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("addYandexBridge: AddBridge: %v", err)
	}
	h.bridge.Reload(bridgeID)
	return bridgeID
}

func TestE2E_BridgeYandex(t *testing.T) {
	t.Run("B-01_WebhookSecretValidation", testYandexB01WebhookSecret)
	t.Run("B-02_IncomingEventFlow", testYandexB02IncomingEventFlow)
	t.Run("B-03_IncomingImage", testYandexB03IncomingImage)
}

func testYandexB01WebhookSecret(t *testing.T) {
	fy := newFakeYandex(t)
	h := setupE2E(t, withHTTPServer(), withBridge(), withFakeYandexReal(fy))

	token := "y01bot:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "y01bot",
		BotUsername:   "y01bot",
		ManageEnabled: true,
	})
	h.fake.RegisterBot(token, "y01bot", 301)

	bridgeID := addYandexBridge(t, h, botID, fy.URL(), testYandexWebhookSecret)
	body := yandexUpdateBody("chat-001", "alice", "hello", 1001)

	respInvalid := postYandexBridgeIncoming(t, h.ts.URL, bridgeID, body, "wrong-secret")
	defer respInvalid.Body.Close()
	_, _ = io.ReadAll(respInvalid.Body)
	if respInvalid.StatusCode != 401 {
		t.Errorf("B-01: expected 401 for invalid secret, got %d", respInvalid.StatusCode)
	}

	respValid := postYandexBridgeIncoming(t, h.ts.URL, bridgeID, body, testYandexWebhookSecret)
	defer respValid.Body.Close()
	_, _ = io.ReadAll(respValid.Body)
	if respValid.StatusCode != 200 {
		t.Errorf("B-01: expected 200 for valid secret, got %d", respValid.StatusCode)
	}
}

func testYandexB02IncomingEventFlow(t *testing.T) {
	fy := newFakeYandex(t)
	h := setupE2E(t, withHTTPServer(), withBridge(), withFakeYandexReal(fy))

	token := "y02bot:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "y02bot",
		BotUsername:   "y02bot",
		ManageEnabled: true,
	})
	h.fake.RegisterBot(token, "y02bot", 302)

	bridgeID := addYandexBridge(t, h, botID, fy.URL(), testYandexWebhookSecret)

	managedBot, err := bot.NewBot(token, h.store, botID, h.fake.URL())
	if err != nil {
		t.Fatalf("B-02: NewBot: %v", err)
	}
	h.bridge.InstallHookOnBot(managedBot)
	h.proxy.RegisterManagedBot(botID, managedBot)

	const ymChatID = "ym-chat-general"
	const msgText = "hello from yandex"
	body := yandexUpdateBody(ymChatID, "alice", msgText, 2001)

	resp := postYandexBridgeIncoming(t, h.ts.URL, bridgeID, body, testYandexWebhookSecret)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("B-02: expected 200, got %d", resp.StatusCode)
	}

	tgChatID, err := h.store.GetBridgeChatMapping(bridgeID, ymChatID)
	if err != nil || tgChatID == 0 {
		t.Fatalf("B-02: bridge chat mapping not found: err=%v chatID=%d", err, tgChatID)
	}

	var stored models.Message
	h.Eventually(func() bool {
		msgs, err := h.store.GetMessages(botID, tgChatID, 10, 0)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.Text == msgText {
				stored = m
				return true
			}
		}
		return false
	}, 2*time.Second, "incoming Yandex message stored")

	if stored.Text != msgText {
		t.Errorf("B-02: stored message text=%q, want %q", stored.Text, msgText)
	}

	const replyText = "bot reply to yandex"
	if err := managedBot.SendMessage(tgChatID, replyText); err != nil {
		t.Fatalf("B-02: SendMessage: %v", err)
	}

	h.Eventually(func() bool {
		return fy.RequestsCountFor("/bot/v1/messages/sendText/") >= 1
	}, 2*time.Second, "sendText call to fake Yandex API")

	postReqs := fy.RequestsFor("/bot/v1/messages/sendText/")
	if len(postReqs) == 0 {
		t.Fatal("B-02: expected at least one sendText request to fake Yandex API")
	}

	var postBody struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(postReqs[0].body, &postBody); err != nil {
		t.Fatalf("B-02: unmarshal sendText body: %v", err)
	}
	if postBody.Text != replyText {
		t.Errorf("B-02: sendText text=%q, want %q", postBody.Text, replyText)
	}
	if postBody.ChatID != ymChatID {
		t.Errorf("B-02: sendText chat_id=%q, want %q", postBody.ChatID, ymChatID)
	}
}

func testYandexB03IncomingImage(t *testing.T) {
	fy := newFakeYandex(t)
	h := setupE2E(t, withHTTPServer(), withBridge(), withFakeYandexReal(fy))

	token := "y03bot:1234567890"
	botID := h.AddBot(models.BotConfig{
		Token:         token,
		Name:          "y03bot",
		BotUsername:   "y03bot",
		ManageEnabled: true,
	})
	h.fake.RegisterBot(token, "y03bot", 303)

	bridgeID := addYandexBridge(t, h, botID, fy.URL(), testYandexWebhookSecret)

	const ymChatID = "ym-chat-media"
	const ymFileID = "ym-file-abc"
	body := yandexImageUpdateBody(ymChatID, 3001, ymFileID, "photo caption")

	resp := postYandexBridgeIncoming(t, h.ts.URL, bridgeID, body, testYandexWebhookSecret)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("B-03: expected 200, got %d", resp.StatusCode)
	}

	tgChatID, err := h.store.GetBridgeChatMapping(bridgeID, ymChatID)
	if err != nil || tgChatID == 0 {
		t.Fatalf("B-03: chat mapping missing: %v", err)
	}

	var stored models.Message
	h.Eventually(func() bool {
		msgs, err := h.store.GetMessages(botID, tgChatID, 10, 0)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.MediaType == "photo" {
				stored = m
				return true
			}
		}
		return false
	}, 2*time.Second, "incoming Yandex image stored")

	wantFileID := bridge.BridgeFileID(bridgeID, ymFileID)
	if stored.FileID != wantFileID {
		t.Errorf("B-03: file_id=%q, want %q", stored.FileID, wantFileID)
	}
	if stored.Text != "photo caption" {
		t.Errorf("B-03: caption=%q, want photo caption", stored.Text)
	}
}
