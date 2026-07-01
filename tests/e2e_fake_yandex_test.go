package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type yandexRequest struct {
	path      string
	body      []byte
	timestamp time.Time
}

// fakeYandex is an httptest-based fake Yandex Messenger Bot API server.
type fakeYandex struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	reqs   []yandexRequest
}

func newFakeYandex(t *testing.T) *fakeYandex {
	t.Helper()
	s := &fakeYandex{t: t}
	s.server = httptest.NewServer(http.HandlerFunc(s.route))
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeYandex) URL() string { return s.server.URL }

func (s *fakeYandex) RequestsFor(path string) []yandexRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []yandexRequest
	for _, r := range s.reqs {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func (s *fakeYandex) RequestsCountFor(path string) int {
	return len(s.RequestsFor(path))
}

func (s *fakeYandex) route(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("fakeYandex: read body: %v", err)
	}

	rec := yandexRequest{
		path:      r.URL.Path,
		body:      body,
		timestamp: time.Now(),
	}

	s.mu.Lock()
	s.reqs = append(s.reqs, rec)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	switch r.URL.Path {
	case "/bot/v1/messages/sendText/":
		var req struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_id": 42})
	case "/bot/v1/messages/sendImage/", "/bot/v1/messages/sendFile/", "/bot/v1/messages/sendGallery/":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_id": 43})
	case "/bot/v1/messages/getFile/":
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok":false,"description":"unknown endpoint"}`))
	}
}

func withFakeYandexReal(fy *fakeYandex) e2eOpt {
	return func(h *e2eHarness) {
		h.fakeYM = fy.server
	}
}
