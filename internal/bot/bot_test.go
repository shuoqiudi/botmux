package bot

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewBotAuthorizationErrorIsSanitized(t *testing.T) {
	const token = "123456:super-secret-token"

	server := httptest.NewServer(nil)
	baseURL := server.URL
	server.Close()

	_, err := NewBot(token, nil, 1, baseURL)
	if err == nil {
		t.Fatal("NewBot() error = nil, want authorization failure")
	}
	if got, want := err.Error(), "Telegram bot authorization failed"; got != want {
		t.Fatalf("NewBot() error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), baseURL) {
		t.Fatalf("NewBot() error contains sensitive request details: %q", err)
	}
}
