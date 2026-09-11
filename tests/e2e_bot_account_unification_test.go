package tests

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

func TestE2E_BotAccountAppearsInBotList(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	const token = "950002:synthetic-alternate"
	h.fake.RegisterBot(token, "alternate_bot", 950002)
	status, _ := serviceCall(t, h, "POST", "/api/gateway/v1/bot-accounts", "", "", map[string]any{"name": "Alternate", "token": token}, true)
	if status != 201 {
		t.Fatalf("create account: status=%d", status)
	}
	status, raw := serviceCall(t, h, "GET", "/api/bots", "", "", nil, true)
	var bots []models.BotConfig
	if err := json.Unmarshal(raw, &bots); err != nil || status != 200 {
		t.Fatalf("list bots: status=%d err=%v", status, err)
	}
	if len(bots) != 1 || bots[0].BotUsername != "alternate_bot" {
		t.Fatalf("Account 列表已有 alternate_bot，Bot 列表应有同一 Bot；实际 Bot 数量=%d", len(bots))
	}
}

func TestE2E_BotListCreatesSubscriptionAccount(t *testing.T) {
	h := setupE2E(t, withHTTPServer())
	const token = "950001:synthetic-primary"
	h.fake.RegisterBot(token, "primary_bot", 950001)
	status, _ := serviceCall(t, h, "POST", "/api/bots/add", "", "", map[string]any{"name": "Primary", "token": token}, true)
	if status != 200 {
		t.Fatalf("create bot: %d", status)
	}
	status, raw := serviceCall(t, h, "GET", "/api/gateway/v1/bot-accounts", "", "", nil, true)
	var accounts []models.BotAccount
	if err := json.Unmarshal(raw, &accounts); err != nil || status != 200 {
		t.Fatalf("accounts: %d %v", status, err)
	}
	if len(accounts) != 1 || accounts[0].Username != "primary_bot" {
		t.Fatalf("Bot 未自动出现在订阅选项；Account 数量=%d", len(accounts))
	}
}

func TestE2E_BotAccountSharedLifecycle(t *testing.T) {
	f := setupSubscription(t)
	f.subscribe(f.destination.ID, 1, 201)
	botID := f.account.NativeBotID
	if botID == 0 {
		t.Fatal("Account 缺少稳定 Bot 关联")
	}
	var alias models.BotAccount
	if err := json.Unmarshal(f.admin("POST", "/api/gateway/v1/bot-accounts", map[string]any{"name": "Old alias", "token": "940001:subscription-secret"}, 201), &alias); err != nil {
		t.Fatal(err)
	}
	if alias.NativeBotID != botID {
		t.Fatal("同一凭据产生了不同 Bot")
	}
	f.admin("POST", "/api/gateway/v1/destinations", map[string]any{"name": "Alias B", "bot_account_id": alias.ID, "chat_id": -100940002}, 201)
	const incomplete = "940001:synthetic-incomplete"
	f.h.fake.RegisterBot(incomplete, "subscriber", 940001)
	f.h.fake.RegisterChat(incomplete, -100940001, "A")
	f.admin("POST", "/api/bots/update", map[string]any{"id": botID, "name": "Must roll back", "token": incomplete}, 422)
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(f.account.ID), map[string]any{"name": "Must roll back", "token": incomplete, "expected_revision": 1}, 422)
	unchanged, err := f.h.store.GetBotAccount(f.account.ID)
	if err != nil || unchanged.Revision != 1 || unchanged.Name != "Subscriber" {
		t.Fatal("验证失败改变了共享配置")
	}
	if err := f.h.store.UpsertChat(botID, models.Chat{ID: -100940002, Type: "group", Title: "Known group"}); err != nil {
		t.Fatal(err)
	}
	f.admin("POST", "/api/bots/update", map[string]any{"id": botID, "name": "Unified name"}, 200)
	for _, id := range []int64{f.account.ID, alias.ID} {
		a, err := f.h.store.GetBotAccount(id)
		if err != nil || a.Name != "Unified name" || a.Revision != 2 {
			t.Fatalf("Bot 改名未同步 Account %d", id)
		}
	}
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(alias.ID), map[string]any{"name": "Stale editor", "expected_revision": 1}, 409)
	const rotated = "940001:synthetic-new-token"
	f.h.fake.RegisterBot(rotated, "subscriber", 940001)
	f.h.fake.RegisterChat(rotated, -100940001, "A")
	f.h.fake.RegisterChat(rotated, -100940002, "B")
	f.admin("POST", "/api/bots/update", map[string]any{"id": botID, "name": "Unified name", "token": rotated}, 200)
	for _, id := range []int64{f.account.ID, alias.ID} {
		a, err := f.h.store.GetBotAccount(id)
		if err != nil || a.Token != rotated || a.NativeBotID != botID {
			t.Fatalf("Bot 换凭据未同步 Account %d", id)
		}
		ids, err := f.h.store.NativeBotIDsForGatewayAccount(id)
		if err != nil || len(ids) != 1 || ids[0] != botID {
			t.Fatal("换凭据改变了关联")
		}
	}
	chats := f.admin("GET", "/api/gateway/v1/bot-accounts/"+itoa(alias.ID)+"/chats", nil, 200)
	if !strings.Contains(string(chats), "Known group") {
		t.Fatal("换凭据丢失已知聊天")
	}
	f.admin("PUT", "/api/gateway/v1/bot-accounts/"+itoa(alias.ID), map[string]any{"name": "From compatibility API", "expected_revision": 3}, 200)
	b, err := f.h.store.GetBotConfig(botID)
	if err != nil || b.Name != "From compatibility API" || b.ManageEnabled || b.ProxyEnabled {
		t.Fatal("Account 编辑未同步 Bot 或改变了运行模式")
	}
	b.ManageEnabled = true
	if err := f.h.store.UpdateBotConfig(*b); err != nil {
		t.Fatal(err)
	}
	if err := f.h.proxy.RestartBot(botID); err != nil {
		t.Fatal(err)
	}
	f.admin("POST", "/api/bots/delete?id="+itoa(botID), nil, 409)
	if !f.h.proxy.IsRunning(botID) {
		t.Fatal("删除被拒绝却停止了 Bot")
	}
	if _, err := f.h.store.GetBotConfig(botID); err != nil {
		t.Fatal("删除保护没有保留 Bot")
	}
	n := f.publish("after-unification", "Still subscribed")
	var recipientAccount int64
	if err := f.h.store.DB().QueryRow(`SELECT bot_account_id FROM gateway_service_recipients WHERE notification_id=?`, n.ID).Scan(&recipientAccount); err != nil {
		t.Fatal(err)
	}
	if n.SubscriptionCount != 1 || recipientAccount != f.account.ID {
		t.Fatal("统一配置改变订阅接收关系")
	}
	for _, path := range []string{"/api/bots", "/api/gateway/v1/bot-accounts"} {
		raw := f.admin("GET", path, nil, 200)
		if strings.Contains(string(raw), rotated) || strings.Contains(string(raw), "token_ciphertext") {
			t.Fatal("列表泄露凭据")
		}
	}
	unused := f.h.AddBot(models.BotConfig{Name: "Unused", Token: "950009:synthetic-unused"})
	f.admin("POST", "/api/bots/delete?id="+itoa(unused), nil, 200)
	accounts, err := f.h.store.GetBotAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.NativeBotID == unused {
			t.Fatal("删除 Bot 留下孤立 Account")
		}
	}
}

func TestBotAccountMigrationPreservesReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := store.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the deployed schema's independent records, including an alias.
	_, err = s.DB().Exec(`INSERT INTO bots(id,name,token,bot_username,manage_enabled,disabled) VALUES(41,'Primary','950001:synthetic-primary','primary_bot',1,1);
 INSERT INTO gateway_bot_accounts(id,name,username,token,created_at,updated_at) VALUES
 (71,'Acceptance primary','primary_bot','950001:synthetic-primary','before','before'),
 (72,'Acceptance alternate','alternate_bot','950002:synthetic-alternate','before','before'),
 (73,'Alternate alias','alternate_bot','950002:synthetic-alternate','before','before');
 INSERT INTO gateway_telegram_destinations(id,name,bot_account_id,chat_id,created_at,updated_at) VALUES(81,'Private',72,950099,'before','before')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err = store.NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		bots, err := s.GetBotConfigs()
		if err != nil {
			t.Fatal(err)
		}
		if len(bots) != 2 {
			s.Close()
			t.Fatalf("历史两种 Telegram Bot 应统一为两项，实际=%d", len(bots))
		}
		primary, err := s.GetBotConfig(41)
		if err != nil || !primary.Disabled || !primary.ManageEnabled {
			t.Fatal("迁移改变现有 Bot 配置")
		}
		for _, id := range []int64{71, 72, 73} {
			ids, err := s.NativeBotIDsForGatewayAccount(id)
			if err != nil || len(ids) != 1 {
				t.Fatalf("历史 Account %d 未稳定关联 Bot: %v", id, ids)
			}
		}
		alternate, err := s.GetBotConfigByToken("950002:synthetic-alternate")
		if err != nil || alternate.ManageEnabled || alternate.ProxyEnabled {
			t.Fatal("迁移不应为发送账号开启轮询")
		}
		d, err := s.GetTelegramDestination(81)
		if err != nil || d.BotAccountID != 72 || d.ChatID != 950099 {
			t.Fatal("迁移改变已有目标引用")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
