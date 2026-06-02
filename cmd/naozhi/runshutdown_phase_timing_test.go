package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunShutdown_EmitsPhaseTimings pins R245-ARCH-38 (#893): the teardown
// path must emit a per-phase timing log line for each ordered stage (sysmgr →
// scheduler → router) plus a total. Without the pin a refactor that drops a
// phase log silently regresses operator observability — a hung scheduler.Stop
// or router.Shutdown becomes invisible in journalctl because the surrounding
// "shutdown starting" / "shutdown complete" lines look identical to a healthy
// teardown.
//
// Post-#1487/#1376 the per-phase slog moved into runShutdownSteps
// (runshutdown.go) keyed on the step name, and the ordered step names live in
// main()'s shutdownStep slice. This pin now verifies (a) runshutdown.go still
// emits a `phase=` timing line, (b) main.go still summarises with total_ms,
// and (c) the step names appear in main.go in the contract order. The
// behavioral call-order guarantee lives in runshutdown_order_test.go.
func TestRunShutdown_EmitsPhaseTimings(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)

	mainBody := readSrc(t, filepath.Join(dir, "main.go"))
	stepBody := readSrc(t, filepath.Join(dir, "runshutdown.go"))

	// runshutdown.go owns the per-phase timing emit + the summary key.
	for _, w := range []struct{ fragment, why string }{
		{`"shutdown phase complete", "phase", s.name`, "per-phase teardown timing emit"},
	} {
		if !strings.Contains(stepBody, w.fragment) {
			t.Errorf("runshutdown.go: missing %s — expected fragment %q for #893", w.why, w.fragment)
		}
	}
	for _, w := range []struct{ fragment, why string }{
		{`"shutdown complete"`, "summary log line with total_ms"},
		{`"total_ms"`, "summary key for total elapsed teardown time"},
	} {
		if !strings.Contains(mainBody, w.fragment) {
			t.Errorf("main.go runShutdown: missing %s — expected fragment %q for #893", w.why, w.fragment)
		}
	}

	// The phase ordering is a contract; verify the named steps appear in
	// main()'s step slice in order so a future shuffle that runs router
	// before sysmgr (which would race with daemon Tick paths) breaks loudly
	// at the source level too.
	idxSys := strings.Index(mainBody, `name: "sysmgr"`)
	idxSched := strings.Index(mainBody, `name: "scheduler"`)
	idxRouter := strings.Index(mainBody, `name: "router"`)
	if idxSys < 0 || idxSched < 0 || idxRouter < 0 {
		t.Fatalf("main.go: missing a named shutdown step (sysmgr=%d scheduler=%d router=%d)",
			idxSys, idxSched, idxRouter)
	}
	if !(idxSys < idxSched && idxSched < idxRouter) {
		t.Errorf("main.go runShutdown: step ordering must be sysmgr → scheduler → router "+
			"(got positions sysmgr=%d scheduler=%d router=%d) — see RFC v2.1 §5.2",
			idxSys, idxSched, idxRouter)
	}
}

func readSrc(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(src)
}
