package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			status, code, err := client.result(context.Background(), 1, tc.identity)
			if tc.valid {
				if err != nil || status != "succeeded" || code != "" {
					t.Fatalf("successful result not recognized: status=%q code=%q err=%v", status, code, err)
				}
			} else if err != errResultUnavailable {
				t.Fatalf("invalid result accepted: %v", err)
			}
		})
	}
}
