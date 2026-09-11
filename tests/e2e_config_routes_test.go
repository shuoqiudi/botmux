package tests

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

func TestE2E_ConfigConditionalRoutesRoundTrip(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	src := registerAndManage(source, "970001:source", "source")
	dst := registerAndManage(source, "970002:target", "target")
	rules := []models.Route{
		{SourceBotID: src, TargetBotID: dst, ConditionType: "text", ConditionValue: "hello", Action: "forward", TargetChatID: -9007199254740993, Enabled: true, Description: "First"},
		{SourceBotID: src, TargetBotID: dst, ConditionType: "text", ConditionValue: "hello", Action: "forward", TargetChatID: -9007199254740993, Enabled: true, Description: "First"},
		{SourceBotID: src, TargetBotID: dst, SourceChatID: -9007199254740993, ConditionType: "chat_id", ConditionValue: "-9007199254740993", Action: "copy", Enabled: false, Description: "Disabled"},
	}
	for i := range rules {
		var err error
		rules[i].ID, err = source.store.AddRoute(rules[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "routes.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export routes: %v %s", err, out)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		ConditionalRoutes []struct {
			Ref, Description string
			SourceChatID     string `json:"source_chat_id"`
			TargetChatID     string `json:"target_chat_id"`
		} `json:"conditional_routes"`
	}
	if err := json.Unmarshal(before, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ConditionalRoutes) != 3 {
		t.Fatal("rules missing")
	}
	routes := snapshot.ConditionalRoutes
	if routes[0].Ref == "" || routes[0].Ref == routes[1].Ref || routes[0].TargetChatID != "-9007199254740993" || routes[0].SourceChatID != "0" || routes[2].SourceChatID != "-9007199254740993" || routes[2].TargetChatID != "0" {
		t.Fatal("identity, sentinel or precision lost")
	}
	target := setupE2E(t, withHTTPServer())
	unused := target.AddBot(models.BotConfig{Name: "Removed", Token: "970009:removed"})
	if err := target.store.DeleteBotConfig(unused); err != nil {
		t.Fatal(err)
	}
	target.fake.RegisterBot("970001:source", "source", 970001)
	target.fake.RegisterBot("970002:target", "target", 970002)
	for i := 0; i < 2; i++ {
		if out, err := configScript(t, target, "restore", file); err != nil {
			t.Fatalf("restore routes: %v %s", err, out)
		}
	}
	if len(target.fake.RequestsFor("sendMessage"))+len(target.fake.RequestsFor("forwardMessage")) != 0 {
		t.Fatal("restore triggered forwarding")
	}
	again := filepath.Join(t.TempDir(), "again.json")
	if out, err := configScript(t, target, "export", again); err != nil {
		t.Fatalf("export target: %v %s", err, out)
	}
	after, _ := os.ReadFile(again)
	if !bytes.Equal(before, after) {
		t.Fatal("round trip changed snapshot")
	}
	bots, err := target.store.GetBotConfigs()
	if err != nil {
		t.Fatal(err)
	}
	var restoredSrc, restoredDst int64
	for _, b := range bots {
		if b.Token == "970001:source" {
			restoredSrc = b.ID
		}
		if b.Token == "970002:target" {
			restoredDst = b.ID
		}
	}
	restored, err := target.store.GetRoutes(restoredSrc)
	if err != nil || len(restored) != 3 {
		t.Fatalf("restored rules: %v %v", restored, err)
	}
	if restoredSrc == src || restored[0].TargetBotID != restoredDst || restored[2].Enabled {
		t.Fatal("Bot references or disabled state lost")
	}
	// Normal page edits retain identities and produce readable field changes.
	restored[0].Description = "Edited description"
	status, _ := serviceCall(t, target, "POST", "/api/routes/update", "", "", restored[0], true)
	if status != 200 {
		t.Fatalf("page edit: %d", status)
	}
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(before), true)
	if status != 409 {
		t.Fatalf("edited rules must conflict: %d", status)
	}
	if out, err := configScript(t, target, "export", again); err != nil {
		t.Fatalf("export edit: %v %s", err, out)
	}
	edited, _ := os.ReadFile(again)
	expected := bytes.Replace(before, []byte(`"description": "First"`), []byte(`"description": "Edited description"`), 1)
	if !bytes.Equal(edited, expected) {
		t.Fatal("page edit changed identity or unrelated configuration")
	}
}

func TestE2E_ConfigConditionalRoutesPreserveBehavior(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	src := source.AddBot(models.BotConfig{Name: "source", Token: "970011:source", ManageEnabled: true, PollingTimeout: 1})
	dst := source.AddBot(models.BotConfig{Name: "target", Token: "970012:target", ManageEnabled: true, PollingTimeout: 1})
	rules := []models.Route{
		{ConditionType: "text", ConditionValue: "hello", Action: "forward", TargetChatID: 201, Enabled: true},
		{ConditionType: "text", ConditionValue: "hello", Action: "forward", TargetChatID: 201, Enabled: true},
		{ConditionType: "text", ConditionValue: "hello", Action: "copy", SourceChatID: 100, TargetChatID: 202, Enabled: true},
		{ConditionType: "text", ConditionValue: "hello", Action: "drop", Enabled: true},
		{ConditionType: "text", ConditionValue: "hello", Action: "forward", TargetChatID: 299, Enabled: true},
		{ConditionType: "user_id", ConditionValue: "42", Action: "copy", TargetChatID: 203, Enabled: true},
		{ConditionType: "chat_id", ConditionValue: "101", Action: "forward", Enabled: true},
		{ConditionType: "text", ConditionValue: "disabled", Action: "forward", TargetChatID: 298, Enabled: false},
	}
	for _, r := range rules {
		r.SourceBotID = src
		r.TargetBotID = dst
		if _, err := source.store.AddRoute(r); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(t.TempDir(), "behavior.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	before, _ := os.ReadFile(file)
	source.proxy.Start()
	run := func(h *e2eHarness, sourceID int64) {
		t.Helper()
		for i, update := range []map[string]any{
			makeTextUpdate(1, 100, 7, "alice", "HELLO"),
			makeTextUpdate(2, 102, 7, "alice", "hello"),
			makeTextUpdate(3, 100, 42, "bob", "user match"),
			makeTextUpdate(4, 101, 7, "alice", "chat match"),
			makeTextUpdate(5, 100, 7, "alice", "no match"),
			makeTextUpdate(6, 100, 7, "alice", "disabled"),
		} {
			h.fake.EnqueueUpdate("970011:source", update)
			h.Eventually(func() bool { b, err := h.store.GetBotConfig(sourceID); return err == nil && b.Offset >= int64(i+2) }, 3*time.Second, "Telegram update should finish processing")
		}
		type expectedRequest struct {
			method string
			chat   int64
		}
		expected := []expectedRequest{{"sendMessage", 201}, {"sendMessage", 201}, {"forwardMessage", 202}, {"sendMessage", 201}, {"sendMessage", 201}, {"forwardMessage", 203}, {"sendMessage", 101}}
		var sent []recordedRequest
		for _, r := range h.fake.Requests() {
			if r.method == "sendMessage" || r.method == "forwardMessage" {
				sent = append(sent, r)
			}
		}
		if len(sent) != len(expected) {
			t.Fatalf("expected %d routing requests, got %d", len(expected), len(sent))
		}
		for i, r := range sent {
			if r.token != "970012:target" || r.method != expected[i].method || parseChatID(nil, r.body) != expected[i].chat {
				t.Fatalf("request %d: wrong Bot, method or destination: %s %s", i, r.method, r.body)
			}
		}
		copyRequest := httptest.NewRequest("POST", "/", bytes.NewReader(sent[2].body))
		copyRequest.Header = sent[2].headers
		if parseIntParam(copyRequest, sent[2].body, "from_chat_id") != 100 || parseIntParam(copyRequest, sent[2].body, "message_id") != 10 {
			t.Fatal("copy lost source message reference")
		}
	}
	run(source, src)
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("export activity: %v %s", err, out)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("routing activity changed configuration")
	}
	target := setupE2E(t, withHTTPServer())
	target.fake.RegisterBot("970011:source", "source", 970011)
	target.fake.RegisterBot("970012:target", "target", 970012)
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	bots, err := target.store.GetBotConfigs()
	if err != nil {
		t.Fatal(err)
	}
	var targetSrc, targetDst int64
	for _, b := range bots {
		if b.Token == "970011:source" {
			targetSrc = b.ID
		} else {
			targetDst = b.ID
		}
	}
	if len(target.fake.RequestsFor("sendMessage"))+len(target.fake.RequestsFor("forwardMessage")) != 0 {
		t.Fatal("restore sent messages")
	}
	// Old reply chains do not transfer, even though source traffic created mappings.
	mapping, err := source.store.FindReverseMapping(dst, 201)
	if err != nil {
		t.Fatal(err)
	}
	oldReply := makeTextUpdate(1, 201, 55, "operator", "old reply")
	oldReply["message"].(map[string]any)["reply_to_message"] = map[string]any{"message_id": float64(mapping.TargetMsgID)}
	target.fake.EnqueueUpdate("970012:target", oldReply)
	target.Eventually(func() bool { b, e := target.store.GetBotConfig(targetDst); return e == nil && b.Offset >= 2 }, 3*time.Second, "old reply processed")
	if len(target.fake.RequestsFor("sendMessage")) != 0 {
		t.Fatal("historical reply mapping migrated")
	}
	run(target, targetSrc)
	// New messages create the usual reverse routing relationship.
	mapping, err = target.store.FindReverseMapping(targetDst, 203)
	if err != nil {
		t.Fatal(err)
	}
	reply := makeTextUpdate(2, 203, 55, "operator", "new reply")
	reply["message"].(map[string]any)["reply_to_message"] = map[string]any{"message_id": float64(mapping.TargetMsgID)}
	target.fake.EnqueueUpdate("970012:target", reply)
	target.Eventually(func() bool { b, e := target.store.GetBotConfig(targetDst); return e == nil && b.Offset >= 3 }, 3*time.Second, "new reply processed")
	sends := target.fake.RequestsFor("sendMessage")
	last := sends[len(sends)-1]
	if len(sends) != 6 || last.token != "970011:source" || parseChatID(nil, last.body) != 100 {
		t.Fatal("new reply did not return through source Bot")
	}
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("activity caused replay conflict: %v %s", err, out)
	}
}

func TestE2E_ConfigConditionalRoutesRejectInvalidSnapshot(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	id := source.AddBot(models.BotConfig{Name: "Validation", Token: "970021:validation"})
	if _, err := source.store.AddRoute(models.Route{SourceBotID: id, TargetBotID: id, ConditionType: "text", ConditionValue: "hello", Action: "forward", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	status, raw := serviceCall(t, source, "GET", "/api/config/export", "", "", nil, true)
	if status != 200 {
		t.Fatalf("export: %d", status)
	}
	target := setupE2E(t, withHTTPServer())
	_, empty := serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
	cases := []struct {
		name, field string
		value       any
	}{
		{"unknown source", "source_bot_ref", "missing"},
		{"unknown target", "target_bot_ref", "missing"},
		{"invalid identity", "ref", "invalid ref"},
		{"unknown condition", "condition_type", "future"},
		{"bad regex", "condition_value", "[secret-do-not-echo"},
		{"unknown action", "action", "future"},
		{"source overflow", "source_chat_id", "-9223372036854775809"},
		{"target overflow", "target_chat_id", "9223372036854775808"},
		{"number instead of string", "target_chat_id", 123},
		{"noncanonical chat", "source_chat_id", "00"},
		{"null enabled", "enabled", nil},
		{"unknown field", "secret-do-not-echo", "value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			r := doc["conditional_routes"].([]any)[0].(map[string]any)
			r[tc.field] = tc.value
			status, response := serviceCall(t, target, "POST", "/api/config/restore", "", "", doc, true)
			if status != 400 {
				t.Fatalf("invalid rule accepted: %d %s", status, response)
			}
			if bytes.Contains(response, []byte("secret-do-not-echo")) {
				t.Fatal("error leaked rejected value/key")
			}
			_, after := serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
			if !bytes.Equal(empty, after) {
				t.Fatal("invalid rule left partial configuration")
			}
		})
	}
	for _, kind := range []string{"duplicate identity", "missing action", "duplicate field", "bad user ID", "bad chat ID"} {
		t.Run(kind, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			r := doc["conditional_routes"].([]any)[0].(map[string]any)
			switch kind {
			case "duplicate identity":
				doc["conditional_routes"] = []any{r, r}
			case "missing action":
				delete(r, "action")
			case "bad user ID":
				r["condition_type"] = "user_id"
				r["condition_value"] = "9223372036854775808"
			case "bad chat ID":
				r["condition_type"] = "chat_id"
				r["condition_value"] = "not-an-id"
			}
			mutated, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "duplicate field" {
				mutated = bytes.Replace(mutated, []byte(`"action":"forward"`), []byte(`"action":"forward","action":"drop"`), 1)
			}
			status, _ := serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(mutated), true)
			if status != 400 {
				t.Fatalf("invalid rule accepted: %d", status)
			}
			_, after := serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
			if !bytes.Equal(empty, after) {
				t.Fatal("invalid snapshot changed target")
			}
		})
	}
	// The CLI preserves safe field diagnostics for rule fields too.
	invalid := bytes.Replace(raw, []byte(`"condition_type": "text"`), []byte(`"condition_type": "unknown"`), 1)
	file := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(file, invalid, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := configScript(t, target, "restore", file)
	if err == nil || !bytes.Contains(out, []byte("snapshot.conditional_routes[0].condition_type")) {
		t.Fatalf("missing safe field location: %v %s", err, out)
	}
}

func TestE2E_ConfigConditionalRoutesRollbackAndMigration(t *testing.T) {
	source := setupE2E(t, withHTTPServer())
	id := source.AddBot(models.BotConfig{Name: "Legacy", Token: "970031:legacy"})
	for _, description := range []string{"First", "Second"} {
		if _, err := source.store.AddRoute(models.Route{SourceBotID: id, TargetBotID: id, ConditionType: "text", ConditionValue: "hello", Action: "forward", Description: description, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce the old schema so real startup must assign persistent identities.
	if _, err := source.store.DB().Exec(`DROP TRIGGER routes_config_ref_insert; DROP INDEX idx_routes_config_ref; ALTER TABLE routes DROP COLUMN config_ref`); err != nil {
		t.Fatal(err)
	}
	reopenConfigHarness(t, source)
	file := filepath.Join(t.TempDir(), "legacy-rules.json")
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("legacy export: %v %s", err, out)
	}
	before, _ := os.ReadFile(file)
	reopenConfigHarness(t, source)
	if out, err := configScript(t, source, "export", file); err != nil {
		t.Fatalf("reopened export: %v %s", err, out)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("migration changed persisted identities on restart")
	}
	target := setupE2E(t, withHTTPServer())
	_, empty := serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
	for _, boundary := range []string{"route", "receipt"} {
		query := `CREATE TRIGGER fail_rule_restore BEFORE INSERT ON routes WHEN NEW.description='Second' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
		if boundary == "receipt" {
			query = `CREATE TRIGGER fail_rule_restore BEFORE INSERT ON configuration_restores BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
		}
		if _, err := target.store.DB().Exec(query); err != nil {
			t.Fatal(err)
		}
		if _, err := configScript(t, target, "restore", file); err == nil {
			t.Fatal("injected restore failure succeeded")
		}
		_, after := serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
		if !bytes.Equal(empty, after) {
			t.Fatalf("%s failure left partial configuration", boundary)
		}
		if _, err := target.store.DB().Exec(`DROP TRIGGER fail_rule_restore`); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := configScript(t, target, "restore", file); err != nil {
		t.Fatalf("retry: %v %s", err, out)
	}
	reopenConfigHarness(t, target)
	status, receipt := serviceCall(t, target, "POST", "/api/config/restore", "", "", json.RawMessage(before), true)
	if status != 200 || !bytes.Contains(receipt, []byte(`"replayed":true`)) || !bytes.Contains(receipt, []byte(`"conditional_routes":2`)) {
		t.Fatalf("rule replay receipt: %d %s", status, receipt)
	}
	_, after = serviceCall(t, target, "GET", "/api/config/export", "", "", nil, true)
	if !bytes.Equal(before, after) {
		t.Fatal("durable replay changed rules")
	}
	// Array order participates in snapshot identity and cannot silently overwrite.
	var doc map[string]any
	if err := json.Unmarshal(before, &doc); err != nil {
		t.Fatal(err)
	}
	rules := doc["conditional_routes"].([]any)
	rules[0], rules[1] = rules[1], rules[0]
	status, _ = serviceCall(t, target, "POST", "/api/config/restore", "", "", doc, true)
	if status != 409 {
		t.Fatalf("reordered rules did not conflict: %d", status)
	}
}
