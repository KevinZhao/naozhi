package selfupdate

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The auto-update self-restart fix (MEDIUM-1): an in-process checker that
// restarts ITSELF must use the fire-and-forget RestartServiceNoWait, never the
// polling RestartService. RestartService's waitServiceActive sees the still-
// running old process (us) as "active" the instant the restart job is queued,
// falsely confirms success, and then deletes the backup right before systemd
// kills us — leaving no rollback artifact if the new binary is bad.
//
// These are source-guard tests (same pattern as cron/notify_background_ctx_test.go):
// the behaviour is a self-kill that can't be exercised in a unit test, so we
// pin the code shape that encodes the invariant. If a refactor regresses the
// restart primitive or re-introduces a backup-delete on the restart path, this
// fails loudly with the rationale.

func readChecker(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("checker.go")
	if err != nil {
		t.Fatalf("read checker.go: %v", err)
	}
	return string(b)
}

// The restart branch must call RestartServiceNoWait, not the polling
// RestartService.
func TestAutoRestart_UsesNoWaitPrimitive(t *testing.T) {
	src := readChecker(t)
	if !strings.Contains(src, "RestartServiceNoWait(ctx)") {
		t.Error("auto-update restart path must call RestartServiceNoWait(ctx) — see MEDIUM-1")
	}
	// RestartService( (the polling variant) must NOT appear: a self-restart
	// can't meaningfully poll for its own liveness.
	if regexp.MustCompile(`\bRestartService\(ctx\)`).MatchString(src) {
		t.Error("checker.go must NOT call the polling RestartService(ctx); self-restart uses RestartServiceNoWait")
	}
}

// The restart branch must NOT delete the backup: it is the only rollback
// artifact if systemd brings up a bad new binary. (The not-running and
// download-mode branches legitimately manage the backup differently; we only
// assert no os.Remove(backupPath) lands AFTER the self-restart trigger.)
func TestAutoRestart_KeepsBackup(t *testing.T) {
	src := readChecker(t)
	idx := strings.Index(src, "RestartServiceNoWait(ctx)")
	if idx < 0 {
		t.Fatal("RestartServiceNoWait(ctx) not found; restart fix missing")
	}
	tail := src[idx:]
	if strings.Contains(tail, "os.Remove(backupPath)") {
		t.Error("backup must be KEPT after self-restart trigger (rollback artifact) — no os.Remove(backupPath) on this path")
	}
}
