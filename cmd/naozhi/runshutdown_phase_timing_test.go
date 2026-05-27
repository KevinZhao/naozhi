package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunShutdown_EmitsPhaseTimings pins R245-ARCH-38 (#893): runShutdown
// must emit a per-phase timing log line for each ordered teardown stage
// (sysmgr → scheduler → router) plus a total. Without the pin a refactor
// that drops a phase log silently regresses operator observability — a
// hung scheduler.Stop or router.Shutdown becomes invisible in journalctl
// because the surrounding "shutdown starting" / "shutdown complete" lines
// look identical to a healthy teardown.
//
// Source-level pin (rather than constructing a real Server, signaling
// SIGTERM, and parsing slog output) keeps the assertion local to the
// runShutdown closure — runtime tests would have to plumb a mock slog
// handler through main(), which is its own architectural mess.
func TestRunShutdown_EmitsPhaseTimings(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)

	src, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	// A simple substring check is enough: each phase log line is unique
	// because the slog key/value pairs are written verbatim.
	wants := []struct {
		fragment string
		why      string
	}{
		{`"phase", "sysmgr"`, "sysession-manager teardown timing"},
		{`"phase", "scheduler"`, "cron scheduler teardown timing"},
		{`"phase", "router"`, "session-router teardown timing"},
		{`"shutdown complete"`, "summary log line with total_ms"},
		{`"total_ms"`, "summary key for total elapsed teardown time"},
	}
	for _, w := range wants {
		if !strings.Contains(body, w.fragment) {
			t.Errorf("main.go runShutdown: missing %s — expected fragment %q for #893",
				w.why, w.fragment)
		}
	}

	// The phase ordering is a contract documented in the in-source
	// comment; verify the three phase emits appear in order so a future
	// shuffle that runs router before sysmgr (which would race with
	// daemon Tick paths) breaks loudly.
	idxSys := strings.Index(body, `"phase", "sysmgr"`)
	idxSched := strings.Index(body, `"phase", "scheduler"`)
	idxRouter := strings.Index(body, `"phase", "router"`)
	if idxSys < 0 || idxSched < 0 || idxRouter < 0 {
		// Already covered by the loop above; defensive guard for the
		// ordering check below.
		return
	}
	if !(idxSys < idxSched && idxSched < idxRouter) {
		t.Errorf("main.go runShutdown: phase log ordering must be sysmgr → scheduler → router "+
			"(got positions sysmgr=%d scheduler=%d router=%d) — see RFC v2.1 §5.2",
			idxSys, idxSched, idxRouter)
	}
}
