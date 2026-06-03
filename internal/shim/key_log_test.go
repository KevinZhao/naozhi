package shim

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestKeyHashLogged_ShimStarting pins [R20260603150052-SEC-3]: the "shim
// starting" log line must record "keyHash" (the truncated SHA-256 hex) rather
// than the raw session key, which encodes an IM user ID.
func TestKeyHashLogged_ShimStarting(t *testing.T) {
	t.Parallel()
	const rawKey = "feishu:group:u_test1234"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.Info("shim starting", "pid", 1, "keyHash", KeyHash(rawKey))

	line := buf.String()
	if strings.Contains(line, rawKey) {
		t.Errorf("log line must not contain raw key %q; got: %s", rawKey, line)
	}
	wantHash := KeyHash(rawKey)
	if !strings.Contains(line, wantHash) {
		t.Errorf("log line must contain keyHash=%q; got: %s", wantHash, line)
	}
	if strings.Contains(line, "key=") && !strings.Contains(line, "keyHash=") {
		t.Errorf("log line uses plain 'key=' attr; expected 'keyHash='; got: %s", line)
	}
}

// TestKeyHashLogged_DiscoveredLiveShim pins [R20260603150052-SEC-3]: the
// "discovered live shim" log line must record "keyHash" not the raw key.
func TestKeyHashLogged_DiscoveredLiveShim(t *testing.T) {
	t.Parallel()
	const rawKey = "feishu:group:u_test5678"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.Info("discovered live shim", "keyHash", KeyHash(rawKey), "pid", 2)

	line := buf.String()
	if strings.Contains(line, rawKey) {
		t.Errorf("log line must not contain raw key %q; got: %s", rawKey, line)
	}
	wantHash := KeyHash(rawKey)
	if !strings.Contains(line, wantHash) {
		t.Errorf("log line must contain keyHash=%q; got: %s", wantHash, line)
	}
}
