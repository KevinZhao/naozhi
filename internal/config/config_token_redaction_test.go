package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// R20260607-SEC-3 (#1889): NodeConfig / UpstreamConfig implement
// slog.LogValuer so a direct slog serialization of the struct redacts the
// bearer Token instead of leaking it. These tests log a populated config
// through a real slog handler and assert the plaintext token never appears
// while the redaction marker does.

func logLine(t *testing.T, key string, v any) string {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{})
	slog.New(h).Info("config", key, v)
	return buf.String()
}

func TestNodeConfig_LogValue_RedactsToken(t *testing.T) {
	t.Parallel()
	cfg := NodeConfig{
		URL:         "https://node.example",
		Token:       "super-secret-bearer-abc123",
		DisplayName: "edge-1",
		Insecure:    false,
	}
	out := logLine(t, "node", cfg)
	if strings.Contains(out, "super-secret-bearer-abc123") {
		t.Errorf("plaintext token leaked into log output: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in log output: %s", out)
	}
	if !strings.Contains(out, "node.example") {
		t.Errorf("non-secret URL should still be logged: %s", out)
	}
}

func TestUpstreamConfig_LogValue_RedactsToken(t *testing.T) {
	t.Parallel()
	cfg := UpstreamConfig{
		URL:         "https://primary.example",
		NodeID:      "n-42",
		Token:       "another-secret-token-xyz789",
		DisplayName: "reverse-1",
	}
	out := logLine(t, "upstream", cfg)
	if strings.Contains(out, "another-secret-token-xyz789") {
		t.Errorf("plaintext token leaked into log output: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in log output: %s", out)
	}
	if !strings.Contains(out, "n-42") {
		t.Errorf("non-secret node_id should still be logged: %s", out)
	}
}

// An empty token must render empty (not "[REDACTED]") so logs distinguish
// "configured but hidden" from "absent".
func TestConfig_LogValue_EmptyTokenNotRedacted(t *testing.T) {
	t.Parallel()
	out := logLine(t, "node", NodeConfig{URL: "http://x", DisplayName: "d"})
	if strings.Contains(out, "[REDACTED]") {
		t.Errorf("empty token should not render [REDACTED]: %s", out)
	}
}
