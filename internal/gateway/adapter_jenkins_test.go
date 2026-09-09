package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestJenkinsResultTimestampPrefix(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte(`{"schema":"it_manage.management-result/v1","request_id":"request-1","delivery_id":"request-1","status":"succeeded","code":""}`))
	for _, tc := range []struct {
		name, prefix, identity string
		valid                  bool
	}{
		{"plain", "", "request-1", true},
		{"jenkins_timestamp", "[2026-09-08T09:35:28.412Z] ", "request-1", true},
		{"wrong_identity", "[2026-09-08T09:35:28.412Z] ", "other-request", false},
		{"arbitrary_prefix", "echo ", "request-1", false},
		{"timestamp_then_arbitrary_prefix", "[2026-09-08T09:35:28.412Z] echo ", "request-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "%sIT_MANAGE_MANAGEMENT_RESULT_BEGIN:%s:IT_MANAGE_MANAGEMENT_RESULT_END\r\n", tc.prefix, raw)
			}))
			defer server.Close()
			client, err := newJenkinsClient(AdapterConfig{JobURL: server.URL + "/job/manage", Username: "test", APIToken: "test", RequestTimeout: time.Second}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.result(context.Background(), 1, tc.identity)
			if tc.valid {
				if err != nil || result.Status != "succeeded" || result.Code != "" {
					t.Fatalf("successful result not recognized: result=%+v err=%v", result, err)
				}
			} else if err != errResultUnavailable {
				t.Fatalf("invalid result accepted: %v", err)
			}
		})
	}
}

func TestJenkinsCommandOutputFixtures(t *testing.T) {
	for _, name := range []string{"success", "failure", "empty", "timeout", "unavailable", "legacy"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../../tests/fixtures/management_result_v1/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var expected managementResult
			if err = json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "IT_MANAGE_MANAGEMENT_RESULT_BEGIN:%s:IT_MANAGE_MANAGEMENT_RESULT_END\n", base64.StdEncoding.EncodeToString(raw))
			}))
			defer server.Close()
			client, err := newJenkinsClient(AdapterConfig{JobURL: server.URL + "/job/manage", Username: "test", APIToken: "test", RequestTimeout: time.Second}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.result(context.Background(), 1, "sample-1253")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != expected.Status || got.Code != expected.Code || string(got.Output) != string(expected.Output) {
				t.Fatal("result contract changed in transit")
			}
		})
	}
}

func TestJenkinsConsoleCapacity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		consoleBytes int
		wantError    bool
	}{
		{"above_previous_limit", 5 << 20, false}, {"at_limit", 8 << 20, false}, {"over_limit", (8 << 20) + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := map[string]any{"format": "text/plain", "encoding": "utf-8", "state": "complete", "reason": "", "text": strings.Repeat("x", 900000)}
			raw, _ := json.Marshal(map[string]any{"schema": managementResultSchema, "request_id": "id", "delivery_id": "id", "status": "succeeded", "code": "", "command_output": output})
			frame := "IT_MANAGE_MANAGEMENT_RESULT_BEGIN:" + base64.StdEncoding.EncodeToString(raw) + ":IT_MANAGE_MANAGEMENT_RESULT_END\n"
			console := strings.Repeat("x", tc.consoleBytes-len(frame)-1) + "\n" + frame
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, console) }))
			defer server.Close()
			client, err := newJenkinsClient(AdapterConfig{JobURL: server.URL + "/job/manage", Username: "test", APIToken: "test", RequestTimeout: time.Second}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.result(context.Background(), 1, "id")
			if tc.wantError {
				if !errors.Is(err, errResultTooLarge) {
					t.Fatalf("want explicit capacity error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got commandOutput
			if err = json.Unmarshal(result.Output, &got); err != nil {
				t.Fatal(err)
			}
			if got.Text == nil || len(*got.Text) != 900000 {
				t.Fatal("large output truncated")
			}
		})
	}
}
