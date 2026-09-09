package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/store"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayAdapterCommandOutput(t *testing.T) {
	h := newAdapterHarness(t, "success")
	h.jenkins.mu.Lock()
	h.jenkins.output = map[string]any{"format": "text/plain", "encoding": "utf-8", "state": "complete", "reason": "", "text": "\x1b[32m名称  <a>&\x1b[0m\r\n"}
	h.jenkins.mu.Unlock()
	id := h.ingest("/it_manage dns list", 9001)
	h.finish(id)
	sent := h.fake.RequestsFor("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("want acknowledgment and output, got %d", len(sent))
	}
	var message map[string]any
	if err := json.Unmarshal(sent[1].body, &message); err != nil {
		t.Fatal(err)
	}
	if message["parse_mode"] != "HTML" || !strings.Contains(message["text"].(string), "<pre>名称  &lt;a&gt;&amp;\n</pre>") {
		t.Fatalf("incorrect command output: %s", sent[1].body)
	}
	if message["chat_id"] != float64(-1234) || message["message_thread_id"] != float64(9) || message["reply_parameters"].(map[string]any)["message_id"] != float64(8) {
		t.Fatal("lost conversation association")
	}
}

func TestGatewayAdapterOutputBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		document   bool
	}{
		{"39_lines", strings.Repeat("row\n", 39), false},
		{"40_lines", strings.Repeat("row\n", 40), false},
		{"41_lines", strings.Repeat("row\n", 41), true},
		{"3499_characters", strings.Repeat("中", 3499), false},
		{"3500_characters", strings.Repeat("中", 3500), false},
		{"3501_characters", strings.Repeat("中", 3501), true},
		{"long_line", strings.Repeat("x", 10000), true},
		{"emoji", strings.Repeat("😀", 1000), false},
		{"emoji_message_limit", strings.Repeat("😀", 2200), true},
		{"escaped_html", strings.Repeat("<&>", 1000), false},
		{"trailing_blank_line", strings.Repeat("row\n", 40) + "\n", true},
		{"crlf", strings.Repeat("row\r\n", 40), false},
		{"cr", strings.Repeat("row\r", 40), false},
		{"ansi", "\x1b]0;hidden\x07\x1b[31m中文\x1b[0m  <a>&\t😀\r\n\x00\x7f", false},
		{"c1", "\u009b31m中文\u009b0m\u009dhidden\u009c\x1bPignored\x1b\\  text", false},
		{"attachment_plain_text", strings.Repeat("\x1b[31m<a>& 中文\x1b[0m\r\n", 41), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdapterHarness(t, "success")
			h.jenkins.mu.Lock()
			h.jenkins.output = map[string]any{"format": "text/plain", "encoding": "utf-8", "state": "complete", "reason": "", "text": tc.text}
			h.jenkins.mu.Unlock()
			id := h.ingest("/it_manage dns list", 9002)
			h.finish(id)
			want := strings.ReplaceAll(strings.ReplaceAll(tc.text, "\r\n", "\n"), "\r", "\n")
			switch tc.name {
			case "ansi":
				want = "中文  <a>&\t😀\n"
			case "c1":
				want = "中文  text"
			case "attachment_plain_text":
				want = strings.Repeat("<a>& 中文\n", 41)
			}
			if tc.document {
				sent := h.fake.RequestsFor("sendDocument")
				if len(sent) != 1 {
					t.Fatalf("want one document, got %d", len(sent))
				}
				req := httptest.NewRequest("POST", "/", bytes.NewReader(sent[0].body))
				req.Header = sent[0].headers
				if err := req.ParseMultipartForm(2 << 20); err != nil {
					t.Fatal(err)
				}
				defer req.MultipartForm.RemoveAll()
				file, header, err := req.FormFile("document")
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				content, err := io.ReadAll(file)
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != want {
					t.Fatalf("attachment content mismatch: got %d bytes want %d", len(content), len(want))
				}
				if header.Filename != "command-output-"+id+".txt" || header.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
					t.Fatal("incorrect file metadata")
				}
				if req.FormValue("chat_id") != "-1234" || req.FormValue("message_thread_id") != "9" || !strings.Contains(req.FormValue("reply_parameters"), `"message_id":8`) || !strings.Contains(req.FormValue("caption"), "succeeded") || !strings.Contains(req.FormValue("caption"), id) {
					t.Fatal("lost attachment correlation")
				}
				if len(h.fake.RequestsFor("sendMessage")) != 1 {
					t.Fatal("unexpected extra body or truncated summary")
				}
			} else {
				sent := h.fake.RequestsFor("sendMessage")
				if len(sent) != 2 {
					t.Fatalf("want two messages got %d", len(sent))
				}
				var message map[string]any
				json.Unmarshal(sent[1].body, &message)
				if message["parse_mode"] != "HTML" || !strings.HasSuffix(message["text"].(string), "<pre>"+html.EscapeString(want)+"</pre>") {
					t.Fatal("incorrect full HTML output")
				}
				if len(h.fake.RequestsFor("sendDocument")) != 0 {
					t.Fatal("unexpected attachment")
				}
			}
		})
	}
}

func TestGatewayAdapterOutputStates(t *testing.T) {
	for _, tc := range []struct {
		name, mode, state, reason, text, want string
		missing                               bool
	}{
		{"empty", "success", "complete", "", "", "Command Output: empty.", false},
		{"missing_text", "success", "complete", "", "", "invalid_output_contract", false},
		{"legacy", "success", "", "", "", "unknown (legacy result)", true},
		{"failed_legacy", "result_failed", "", "", "", "unknown (legacy result)", true},
		{"missing_result", "missing_result", "", "", "", "Command Output unavailable.", true},
		{"poll_failure", "result_failed", "partial", "result_unavailable", "previously acquired <output>", "<pre>previously acquired &lt;output&gt;</pre>", false},
		{"unavailable", "success", "unavailable", "result_unavailable", "", "unavailable (result_unavailable)", false},
		{"capacity", "success", "unavailable", "output_limit_exceeded", "", "unavailable (output_limit_exceeded)", false},
		{"failure", "business_failed", "complete", "", "business error <x>", "<pre>business error &lt;x&gt;</pre>", false},
		{"timeout", "business_timeout", "partial", "execution_timeout", "已取得\n", "partial: execution_timeout", false},
		{"partial_empty", "business_timeout", "partial", "execution_timeout", "", "Command Output (partial: execution_timeout): empty.", false},
		{"invalid", "success", "invented", "secret-reason", "not trustworthy", "invalid_output_contract", false},
		{"oversized_contract", "success", "complete", "", strings.Repeat("x", 1<<20), "output_limit_exceeded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdapterHarness(t, tc.mode)
			if !tc.missing {
				h.jenkins.mu.Lock()
				h.jenkins.output = map[string]any{"format": "text/plain", "encoding": "utf-8", "state": tc.state, "reason": tc.reason, "text": tc.text}
				if tc.name == "missing_text" {
					delete(h.jenkins.output.(map[string]any), "text")
				}
				h.jenkins.mu.Unlock()
			}
			id := h.ingest("/it_manage dns list", 9003)
			h.finish(id)
			sent := h.fake.RequestsFor("sendMessage")
			if len(sent) != 2 {
				t.Fatalf("want two messages got %d", len(sent))
			}
			var msg map[string]any
			json.Unmarshal(sent[1].body, &msg)
			text := msg["text"].(string)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("incorrect status/output: %s", text)
			}
			if strings.Contains(text, "secret-reason") {
				t.Fatal("unvalidated reason leaked")
			}
			status := "succeeded"
			if tc.mode == "result_failed" || tc.mode == "missing_result" {
				status = "failed"
			}
			if tc.mode == "business_failed" {
				status = "business_failed"
			}
			if tc.mode == "business_timeout" {
				status = "timeout"
			}
			if !strings.Contains(text, "Request "+status+".") {
				t.Fatal("output state changed business status")
			}
			if tc.name == "timeout" && !strings.Contains(text, "<pre>已取得\n</pre>") {
				t.Fatal("timeout lost acquired output")
			}
		})
	}
}

func TestGatewayAdapterOutputRecovery(t *testing.T) {
	for _, phase := range []string{"result_obtained", "attachment_enqueued", "capacity_enqueued"} {
		t.Run(phase, func(t *testing.T) {
			h := newAdapterHarness(t, "success")
			h.stop()
			output := strings.Repeat("完整 <记录>&\n", 41)
			if phase == "capacity_enqueued" {
				output = strings.Repeat("x", 900000) + "\n末尾<&>"
			}
			h.jenkins.mu.Lock()
			h.jenkins.output = map[string]any{"format": "text/plain", "encoding": "utf-8", "state": "complete", "reason": "", "text": output}
			h.jenkins.mu.Unlock()
			ctx := context.Background()
			h.outbound = gateway.NewService(h.store, newMemoryOutboundQueue(), gateway.NewTelegramHTTPClient(h.fake.URL()), "")
			h.outbound.Start(ctx)
			adapter, err := gateway.NewAdapter(h.store, h.outbound, h.config, nil)
			if err != nil {
				t.Fatal(err)
			}
			id := h.ingest("/it_manage dns list", 9004)
			d, err := h.store.GetInboundDelivery(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			terminal := false
			for time.Now().Before(deadline) {
				if _, _, err = adapter.Process(ctx, d); err != nil {
					t.Fatal(err)
				}
				state, err := h.store.GetAdapterExecution(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if state.Stage == "terminal" {
					terminal = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			h.outbound.Stop()
			if !terminal {
				t.Fatal("did not persist result")
			}
			if strings.HasSuffix(phase, "enqueued") {
				time.Sleep(20 * time.Millisecond)
				if _, _, err = adapter.Process(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			if len(h.fake.RequestsFor("sendDocument")) != 0 {
				t.Fatal("document sent before restart")
			}
			state, err := h.store.GetAdapterExecution(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(state)
			if bytes.Contains(raw, []byte("记录")) || bytes.Contains(raw, []byte("reply_payload")) {
				t.Fatal("output exposed in operational state")
			}
			if err = h.store.Close(); err != nil {
				t.Fatal(err)
			}
			h.store, err = store.NewStore(h.path)
			if err != nil {
				t.Fatal(err)
			}
			route, err := h.store.GetBusinessRoute("it_manage")
			if err != nil {
				t.Fatal(err)
			}
			dest, err := h.store.GetTelegramDestination(route.DestinationID)
			if err != nil {
				t.Fatal(err)
			}
			dest.ChatID = -9999
			if err = h.store.MigrateTelegramDestination(*dest, dest.Revision, "test"); err != nil {
				t.Fatal(err)
			}
			h.jenkins.setMode("missing_result") // Recovery must use durable output, not refetch it.
			h.start()
			h.finish(id)
			if h.jenkins.triggerCount() != 1 {
				t.Fatal("recovery repeated business execution")
			}
			sent := h.fake.RequestsFor("sendDocument")
			if len(sent) != 1 {
				t.Fatalf("want one recovered document, got %d", len(sent))
			}
			req := httptest.NewRequest("POST", "/", bytes.NewReader(sent[0].body))
			req.Header = sent[0].headers
			if err = req.ParseMultipartForm(2 << 20); err != nil {
				t.Fatal(err)
			}
			defer req.MultipartForm.RemoveAll()
			f, _, err := req.FormFile("document")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			raw, err = io.ReadAll(f)
			if err != nil || string(raw) != output || req.FormValue("chat_id") != "-1234" || req.FormValue("message_thread_id") != "9" || !strings.Contains(req.FormValue("reply_parameters"), `"message_id":8`) {
				t.Fatal("recovered attachment content or conversation changed")
			}
		})
	}
}

func TestGatewayAdapterDocumentSendFailures(t *testing.T) {
	for _, mode := range []string{"rate_limit_restart", "ambiguous_restart", "permanent"} {
		t.Run(mode, func(t *testing.T) {
			h := newAdapterHarness(t, "success")
			h.stop()
			h.config.ReplyTimeout = 10 * time.Second
			queue, queueErr := gateway.NewRedisQueue(context.Background(), gateway.RedisConfig{Addr: "127.0.0.1:6379", DB: 15})
			if queueErr != nil {
				t.Fatal(queueErr)
			}
			t.Cleanup(func() { queue.Close() })
			h.outboundQueue = queue
			h.jenkins.mu.Lock()
			h.jenkins.output = map[string]any{"format": "text/plain", "encoding": "utf-8", "state": "complete", "reason": "", "text": strings.Repeat("row\n", 41)}
			h.jenkins.mu.Unlock()
			var attempts atomic.Int32
			h.fake.SetHandler("sendDocument", func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				if mode == "ambiguous_restart" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				if mode == "permanent" {
					w.WriteHeader(403)
					io.WriteString(w, `{"ok":false,"error_code":403}`)
					return
				}
				if n == 1 {
					w.WriteHeader(429)
					io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)
					return
				}
				io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			})
			h.start()
			id := h.ingest("/it_manage dns list", 9005)
			if mode == "rate_limit_restart" {
				deadline := time.Now().Add(5 * time.Second)
				retrying := false
				for time.Now().Before(deadline) {
					m, err := h.store.GetGatewayRouteMetrics(context.Background(), "it_manage", time.Now())
					if err == nil && m.Counts.Retrying == 1 {
						retrying = true
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !retrying {
					t.Fatal("send did not enter retrying")
				}
				h.stop()
				if err := h.store.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				h.store, err = store.NewStore(h.path)
				if err != nil {
					t.Fatal(err)
				}
				h.start()
			}
			h.finish(id)
			state, err := h.store.GetAdapterExecution(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if state.ResultStatus != "succeeded" || state.ResultCode != "" || h.jenkins.triggerCount() != 1 {
				t.Fatal("send failure changed business outcome or repeated execution")
			}
			if mode == "ambiguous_restart" {
				h.stop()
				h.start()
				h.finish(id)
				metrics, err := h.store.GetGatewayRouteMetrics(context.Background(), "it_manage", time.Now())
				if err != nil || metrics.Counts.Reconciling != 1 {
					t.Fatal("ambiguous document not retained for reconciliation")
				}
			}
			want := int32(1)
			if mode == "rate_limit_restart" {
				want = 2
			}
			if attempts.Load() != want {
				t.Fatalf("send attempts=%d want=%d", attempts.Load(), want)
			}
		})
	}
}

func TestGatewayAdapterLegacyEnqueuedReplyRecovery(t *testing.T) {
	h := newAdapterHarness(t, "success")
	h.stop()
	ctx := context.Background()
	id := h.ingest("/it_manage dns list", 9006)
	state, err := h.store.ClaimAdapterExecution(ctx, id, "old-worker", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state.Stage = "terminal"
	state.ResultStatus = "succeeded"
	state.StageStartedAt = time.Now().UnixMilli()
	if err = h.store.SaveAdapterExecution(ctx, state, "old-worker", true); err != nil {
		t.Fatal(err)
	}
	text := "Request succeeded. Request: " + id
	raw, _ := json.Marshal(map[string]any{"kind": "message", "message": map[string]any{"text": text, "message_thread_id": 9, "reply_parameters": map[string]any{"message_id": 8, "allow_sending_without_reply": true}}})
	if _, err = h.store.CreateAdapterReply(ctx, id, "result", raw); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.finish(id)
	sent := h.fake.RequestsFor("sendMessage")
	if len(sent) != 1 {
		t.Fatal("legacy pending reply lost or duplicated")
	}
	var message map[string]any
	json.Unmarshal(sent[0].body, &message)
	if message["text"] != text || h.jenkins.triggerCount() != 0 {
		t.Fatal("upgrade changed an already-persisted reply or triggered business")
	}
}
