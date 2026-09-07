package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTelegramTokenReadsDedicatedFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "telegram-token")
	if err := os.WriteFile(tokenPath, []byte("123456:secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	token, err := resolveTelegramToken("", tokenPath, "ignored-environment-token")
	if err != nil {
		t.Fatalf("resolveTelegramToken: %v", err)
	}
	if token != "123456:secret-token" {
		t.Fatal("token file value was not returned")
	}
}

func TestResolveTelegramTokenRejectsAmbiguousOrMultilineInput(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "telegram-token")
	if err := os.WriteFile(tokenPath, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveTelegramToken("flag-token", tokenPath, ""); err == nil {
		t.Fatal("expected flag and file conflict")
	}
	if _, err := resolveTelegramToken("", tokenPath, ""); err == nil {
		t.Fatal("expected multiline token file rejection")
	}
}
