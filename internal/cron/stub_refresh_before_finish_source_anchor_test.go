package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestErrorPaths_StubRefreshBeforeFinishRun_SourceAnchor is the
// R202606h-GO-009/GO-010 source anchor: on the fresh-context error and cancel
// paths, the sidebar stub re-registration (stubRefresh.run()) MUST run BEFORE
// the finishRun that releases the inflight CAS gate (finishRun →
// finalizer.finalize() → running.Store(false)).
//
// Why: a late stubRefresh.run() AFTER the gate is freed opens a window where a
// concurrent TriggerNow wins the CAS, runs its own preflight Reset +
// GetOrCreate (spawning run-B's live session and registering its live sidebar
// stub), and then run-A's stale-chain stubRefresh.run() blindly overwrites that
// live stub with snap-time lastSessionID — a phantom sidebar row pointing at
// the PRIOR session's JSONL. This mirrors the success-path contract
// (R050103A-COUPLING-1 / #1911), where reapFreshSessionLocked (Reset + stub
// re-register) precedes finishRun; the error paths must follow the same rule.
//
// A true concurrency race for the finalize()→stubRefresh.run() window is hard
// to reproduce deterministically without injecting a hook in the gap, so this
// pins the contract structurally: every stubRefresh.run() / a.stubRefresh.run()
// call must be followed (in source order) by a finishRun(finishArgs{...}) call
// before the next stubRefresh.run() appears. A regression that moves any
// stub-refresh call back below its finishRun fails here without needing an
// end-to-end race.
func TestErrorPaths_StubRefreshBeforeFinishRun_SourceAnchor(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	// Collect the source offsets of every error/cancel-path stub refresh call
	// (both the value receiver `stubRefresh.run()` in execSendError and the
	// struct-field `a.stubRefresh.run()` in executeGetSession) and every
	// finishRun(finishArgs{...}) terminal call.
	reStub := regexp.MustCompile(`(?:a\.)?stubRefresh\.run\(\)`)
	stubMatches := reStub.FindAllStringIndex(body, -1)
	if len(stubMatches) == 0 {
		t.Fatal("scheduler_run.go: no stubRefresh.run() call found — refactor removed the error-path stub re-register?")
	}
	finishMatches := regexp.MustCompile(`s\.finishRun\(finishArgs\{`).FindAllStringIndex(body, -1)
	if len(finishMatches) == 0 {
		t.Fatal("scheduler_run.go: no finishRun(finishArgs{...}) call found")
	}

	// The four error/cancel paths (execSendError cancel + non-cancel,
	// executeGetSession cancel + non-cancel) each call stubRefresh.run() once.
	// execPrepareSpawn's preflight failure also calls stubRefresh.run() but its
	// finishRun fires INSIDE freshContextPreflightP0 (not at the call site), so
	// that call legitimately has no following finishRun in this file's
	// call-site set within its own branch — exclude it by requiring only that
	// each stub call which is part of a finishRun-bearing branch precedes a
	// finishRun. We assert the structural property per stub call: for every
	// stub-refresh call, the nearest finishRun(finishArgs{...}) that shares the
	// branch must appear AFTER it (not before). We approximate "shares the
	// branch" by: there exists a finishRun between this stub call and the next
	// stub call (or EOF). The execPrepareSpawn preflight call is the only one
	// whose following region up to the next stub call contains no finishRun,
	// and it is allowed because finishRun ran inside the helper.
	//
	// To keep the guard precise for the FOUR fixed call sites, we require: the
	// COUNT of stub calls immediately followed (before the next stub call) by a
	// finishRun is at least 4. Pre-fix, those four had finishRun BEFORE the stub
	// (so the region after each stub up to the next stub had no finishRun) and
	// this count would be < 4.
	stubBeforeFinish := 0
	for i, sm := range stubMatches {
		regionEnd := len(body)
		if i+1 < len(stubMatches) {
			regionEnd = stubMatches[i+1][0]
		}
		for _, fm := range finishMatches {
			if fm[0] > sm[0] && fm[0] < regionEnd {
				stubBeforeFinish++
				break
			}
		}
	}
	if stubBeforeFinish < 4 {
		t.Errorf("scheduler_run.go: only %d stubRefresh.run() calls precede their branch finishRun; expected >=4 (the execSendError + executeGetSession cancel/non-cancel paths). A stub refresh placed AFTER finishRun releases the CAS gate, reopening the phantom-stub race (R202606h-GO-009/GO-010).", stubBeforeFinish)
	}
}
