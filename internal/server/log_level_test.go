package server

import (
	"os"
	"strings"
	"testing"
)

// TestSendSessionSpawned_IsDebug locks R84's log-level demotion for
// "send: session spawned" in sessionSend. Without a per-turn Hub test
// harness (routing, project manager, session router) it is not practical
// to exercise sessionSend end-to-end just to probe a log level, so this
// test is a static-source contract check: if a future maintainer
// re-promotes the line back to slog.Info, grep catches it.
//
// The intent is the same as TestGetOrCreate_CreatingNewSession_IsDebug
// in the session package — every per-spawn log row should either be at
// Debug or structurally deduplicated with router.spawnSession's
// "session spawned" Info row. The source check here is a second layer
// of defence so a copy-edit that bypasses the router-level regression
// test still trips on the server-level one.
func TestSendSessionSpawned_IsDebug(t *testing.T) {
	src, err := os.ReadFile("send.go")
	if err != nil {
		t.Fatalf("read send.go: %v", err)
	}
	content := string(src)

	// Required: the Debug form must be present.
	if !strings.Contains(content, `slog.Debug("send: session spawned"`) {
		t.Error(`send.go should log "send: session spawned" at Debug, not Info`)
	}

	// Forbidden: the Info form must be absent. A future copy-edit that
	// re-promotes to Info for "operator visibility" would trip this check.
	if strings.Contains(content, `slog.Info("send: session spawned"`) {
		t.Error(`send.go re-promoted "send: session spawned" to Info — duplicates router.go's "session spawned" Info row`)
	}
}

// TestSendTurnComplete_IsDebug locks that the per-turn trailing log row
// stays at Debug. "turn complete" fires on EVERY successful turn — at
// Info it would be the single loudest journal line on a busy deployment.
// This was already Debug before R84 but we lock it to prevent drift.
func TestSendTurnComplete_IsDebug(t *testing.T) {
	src, err := os.ReadFile("send.go")
	if err != nil {
		t.Fatalf("read send.go: %v", err)
	}
	content := string(src)

	if !strings.Contains(content, `slog.Debug("send: turn complete"`) {
		t.Error(`send.go should log "send: turn complete" at Debug`)
	}
	if strings.Contains(content, `slog.Info("send: turn complete"`) {
		t.Error(`send.go "send: turn complete" must not be at Info — fires on every successful turn`)
	}
}
