// A synthetic Telegram endpoint for testing the packaged process, with no real credentials.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var result any = true
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			result = map[string]any{"id": 123456, "is_bot": true, "first_name": "Fixture", "username": "fixture_bot"}
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			time.Sleep(100 * time.Millisecond)
			result = []any{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	})
	if err := http.ListenAndServe(":18080", nil); err != nil {
		panic(err)
	}
}
