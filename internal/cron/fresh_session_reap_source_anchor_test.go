package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestExecuteOpt_FreshReap_SourceAnchor is the R050103A-COUPLING-1 (#1911)
// source anchor: the fresh-context reap (Reset + stillExists + stub
// re-register) MUST be a single named unit (reapFreshSessionLocked) invoked
// from executeOpt BEFORE finishRun releases the inflight CAS gate. The
// ordering is a correctness invariant — a late Reset after the gate is freed
// could tear down a concurrent TriggerNow's fresh session (run-A clobbering
// run-B). Previously this contract lived only as inline statements guarded by
// a comment, with no compile/test guard against a future reorder.
//
// This pins the structure: (1) the helper is defined, (2) executeOpt calls it
// behind the `snap.fresh` gate, and (3) that call appears before the
// finishRun(finishArgs{...}) call in source order. Any reorder that moves the
// reap after finishRun, or that inlines + reorders the Reset, fails here
// without needing an end-to-end concurrency race to reproduce.
func TestExecuteOpt_FreshReap_SourceAnchor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	// (1) The named helper must exist — the contract's structural home.
	reHelperDef := regexp.MustCompile(`func \(s \*Scheduler\) reapFreshSessionLocked\(`)
	if !reHelperDef.MatchString(body) {
		t.Error("scheduler_run.go missing reapFreshSessionLocked helper (R050103A-COUPLING-1 #1911 structural guard removed)")
	}

	// (2) executeOpt must invoke it behind the snap.fresh gate.
	reGatedCall := regexp.MustCompile(`(?s)if\s+snap\.fresh\s*\{\s*s\.reapFreshSessionLocked\(`)
	if !reGatedCall.MatchString(body) {
		t.Error("scheduler_run.go: reapFreshSessionLocked must be called inside the `if snap.fresh` gate")
	}

	// (3) ORDERING: the reap call must precede ITS finishRun — the success-path
	// finishRun(finishArgs{...}) that releases the CAS gate. executeOpt has
	// several finishRun call sites (error / cancel / shutdown paths) BEFORE the
	// reap, so we cannot compare against the first one. Instead we require that
	// at least one finishRun(finishArgs{...}) call follows the reap in source
	// order. A regression that moves the reap below ALL finishRun calls (CAS
	// released before Reset) leaves no finishRun after it and fails here.
	idxReap := regexp.MustCompile(`s\.reapFreshSessionLocked\(`).FindStringIndex(body)
	if idxReap == nil {
		t.Fatal("scheduler_run.go: no reapFreshSessionLocked call found")
	}
	finishMatches := regexp.MustCompile(`s\.finishRun\(finishArgs\{`).FindAllStringIndex(body, -1)
	if len(finishMatches) == 0 {
		t.Fatal("scheduler_run.go: no finishRun(finishArgs{...}) call found")
	}
	finishAfterReap := false
	for _, m := range finishMatches {
		if m[0] > idxReap[0] {
			finishAfterReap = true
			break
		}
	}
	if !finishAfterReap {
		t.Errorf("scheduler_run.go: no finishRun(finishArgs{...}) follows reapFreshSessionLocked (idx %d); the reap MUST run before the success-path finishRun releases the CAS gate (R050103A-COUPLING-1 #1911)",
			idxReap[0])
	}
}
