package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStopCtx_ReadsConfinedToCallbackPaths pins R249-ARCH-4 (#972): storing
// the lifecycle context in the Scheduler.stopCtx field is a deliberate
// ctx-as-arg exception — it is only justified on paths reachable from a
// robfig/cron callback (which has no ctx parameter slot) where threading a
// ctx argument is impossible.
//
// The risk the field shape introduces is that a future method which DOES
// already receive a ctx argument reaches for s.stopCtx out of convenience,
// silently re-introducing the ctx-as-arg anti-pattern this field's godoc
// warns about. This test confines real (non-comment) `s.stopCtx` reads to
// the allowlist of files that own a callback-derived path. A new read in
// any other file fails CI and forces the author to either thread ctx as an
// argument or extend this allowlist with an explicit justification.
func TestStopCtx_ReadsConfinedToCallbackPaths(t *testing.T) {
	t.Parallel()

	// Files whose code legitimately reads s.stopCtx because their work is
	// dispatched from a robfig/cron callback (execute / tick / notify) or
	// the scheduler's own lifecycle (cold-start GC), none of which can
	// accept a ctx parameter.
	allow := map[string]bool{
		"scheduler.go":        true, // cold-start GC trimAllCtx(s.stopCtx, …)
		"scheduler_run.go":    true, // execute / jitter / spawn / send budget
		"scheduler_notify.go": true, // notify replyCtx parents on s.stopCtx
		"sandbox.go":          true, // executeSandbox run budget — same robfig-callback path as scheduler_run.go's executeOpt (no ctx parameter slot)
	}

	dir := stopCtxTestDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cron pkg dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if allow[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx] // strip line comments
			}
			if strings.Contains(code, "s.stopCtx") {
				t.Errorf("%s:%d reads s.stopCtx outside the callback-path allowlist.\n"+
					"  Line: %s\n"+
					"  s.stopCtx is a ctx-as-arg exception justified only on robfig/cron\n"+
					"  callback-derived paths (R249-ARCH-4 / #972). If this method already\n"+
					"  receives a ctx argument, thread that instead. If it is genuinely a\n"+
					"  new callback path, add %q to the allowlist in this test with a note.",
					name, i+1, strings.TrimSpace(line), name)
			}
		}
	}
}

// stopCtxTestDir returns the directory containing this test file so the scan
// is resilient to `go test` being invoked from any working directory.
func stopCtxTestDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate cron package dir")
	}
	return filepath.Dir(thisFile)
}
