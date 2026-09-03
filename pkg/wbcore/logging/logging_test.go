package logging

import (
	"testing"
)

func TestRedact(t *testing.T) {
	ClearSecrets()
	RegisterSecret("my-super-secret-password-12345")

	// 実値によるマスク
	text := "login with my-super-secret-password-12345 succeeded"
	redacted := Redact(text)
	if redacted != "login with ***REDACTED*** succeeded" {
		t.Fatalf("expected redacted password, got %s", redacted)
	}

	// パターンマッチによるマスク
	kvText := `{"x-app-key": "some-secret-token", "status": "ok"}`
	redactedKV := Redact(kvText)
	if redactedKV != `{"x-app-key": "***REDACTED***", "status": "ok"}` {
		t.Fatalf("expected redacted kv, got %s", redactedKV)
	}
}
