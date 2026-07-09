package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEnvOverlay_LiteralAndFileExpansion(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "anthropic.token")
	if err := os.WriteFile(tokenPath, []byte("sk-secret-123\n\n"), 0600); err != nil {
		t.Fatal(err)
	}

	overlay, err := resolveEnvOverlay(map[string]string{
		"CLAUDE_CODE_USE_BEDROCK":   "0",
		"ANTHROPIC_BASE_URL":        "https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN_FILE": tokenPath,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overlay["CLAUDE_CODE_USE_BEDROCK"] != "0" {
		t.Errorf("literal not copied: %q", overlay["CLAUDE_CODE_USE_BEDROCK"])
	}
	if overlay["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Errorf("literal url not copied: %q", overlay["ANTHROPIC_BASE_URL"])
	}
	// *_FILE expands to the concrete key with trailing newlines trimmed.
	if overlay["ANTHROPIC_AUTH_TOKEN"] != "sk-secret-123" {
		t.Errorf("token not expanded/trimmed: %q", overlay["ANTHROPIC_AUTH_TOKEN"])
	}
	// The *_FILE key itself must NOT be forwarded to the subprocess.
	if _, ok := overlay["ANTHROPIC_AUTH_TOKEN_FILE"]; ok {
		t.Errorf("*_FILE key leaked into overlay")
	}
}

func TestResolveEnvOverlay_MissingFileFailsLoud(t *testing.T) {
	_, err := resolveEnvOverlay(map[string]string{
		"ANTHROPIC_AUTH_TOKEN_FILE": "/nonexistent/path/to/token",
	})
	if err == nil {
		t.Fatal("expected fail-loud error for missing *_FILE, got nil")
	}
}

func TestResolveEnvOverlay_EmptyReturnsNil(t *testing.T) {
	got, err := resolveEnvOverlay(nil)
	if err != nil || got != nil {
		t.Errorf("empty overlay: got (%v, %v), want (nil, nil)", got, err)
	}
}
