package cron

import (
	"os"
	"regexp"
	"testing"
)

// TestReapFreshSessionLocked_RegisterStillExistsRecheck pins the internal
// structure of reapFreshSessionLocked (R090135-ARCH-2): after the Reset call,
// the function MUST re-acquire s.mu.RLock, re-read the jobs map to check
// stillExists, and call registerStubByValue only inside the true branch of that
// check. This mirrors the orphan guard in freshContextPreflightP0.
//
// Rationale: DeleteJobByID's teardown (deleteJobPostCleanup → resetRouterStub)
// does NOT hold the inflight CAS gate, so it can race with the success tail of
// reapFreshSessionLocked. If the re-check were removed (e.g. the Reset and
// register inlined and the stillExists guard dropped), a concurrent Delete would
// resurrect a sidebar stub for a deleted job (zombie row). The guard is a
// correctness invariant that cannot be tested with a race detector alone because
// the window is sub-instruction; a source-anchor test is the appropriate pin.
//
// Pattern mirrors fresh_context_reset_atomic_test.go (lock-pair pin) and
// fresh_session_reap_source_anchor_test.go (ordering pin).
func TestReapFreshSessionLocked_RegisterStillExistsRecheck(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	// Locate the reapFreshSessionLocked function body. We do this by finding the
	// function header and taking everything up to (but not including) the next
	// top-level function definition.
	fnHeaderRe := regexp.MustCompile(`func \(s \*Scheduler\) reapFreshSessionLocked\(`)
	headerIdx := fnHeaderRe.FindStringIndex(body)
	if headerIdx == nil {
		t.Fatal("scheduler_run.go: reapFreshSessionLocked function not found")
	}

	// Find the next top-level func definition after reapFreshSessionLocked to
	// bound the search window to only that function's body.
	nextFnRe := regexp.MustCompile(`\nfunc `)
	nextFnIdxs := nextFnRe.FindAllStringIndex(body[headerIdx[0]:], -1)
	var fnBody string
	if len(nextFnIdxs) > 0 {
		end := headerIdx[0] + nextFnIdxs[0][0]
		fnBody = body[headerIdx[0]:end]
	} else {
		fnBody = body[headerIdx[0]:]
	}

	// (1) The function must call s.router.Reset BEFORE the RLock re-check.
	resetRe := regexp.MustCompile(`s\.router\.Reset\(`)
	rlockRe := regexp.MustCompile(`s\.mu\.RLock\(\)`)
	resetMatch := resetRe.FindStringIndex(fnBody)
	rlockMatch := rlockRe.FindStringIndex(fnBody)
	if resetMatch == nil {
		t.Error("reapFreshSessionLocked: s.router.Reset call not found in function body")
	}
	if rlockMatch == nil {
		t.Error("reapFreshSessionLocked: s.mu.RLock() re-read not found in function body; " +
			"the still-exists guard requires a fresh RLock after Reset")
	}
	if resetMatch != nil && rlockMatch != nil && rlockMatch[0] < resetMatch[0] {
		t.Error("reapFreshSessionLocked: s.mu.RLock() appears BEFORE s.router.Reset; " +
			"the re-check lock must be acquired AFTER Reset so a concurrent Delete is observed correctly")
	}

	// (2) stillExists variable must be populated from the jobs map re-read.
	stillExistsRe := regexp.MustCompile(`stillExists`)
	if !stillExistsRe.MatchString(fnBody) {
		t.Error("reapFreshSessionLocked: stillExists variable not found; " +
			"the orphan guard requires a post-Reset jobs map re-check (mirrors preflight)")
	}

	// (3) registerStubByValue must appear INSIDE an `if stillExists` true branch —
	// i.e., after "if stillExists {" and before the matching else/closing brace.
	// We verify ordering: the `if stillExists` check precedes registerStubByValue.
	ifStillExistsRe := regexp.MustCompile(`if stillExists\s*\{`)
	registerRe := regexp.MustCompile(`s\.registerStubByValue\(`)
	ifMatch := ifStillExistsRe.FindStringIndex(fnBody)
	registerMatch := registerRe.FindStringIndex(fnBody)
	if ifMatch == nil {
		t.Error("reapFreshSessionLocked: `if stillExists {` guard not found; " +
			"registerStubByValue must be gated on the post-Reset still-exists check")
	}
	if registerMatch == nil {
		t.Error("reapFreshSessionLocked: s.registerStubByValue call not found in function body")
	}
	if ifMatch != nil && registerMatch != nil && registerMatch[0] < ifMatch[0] {
		t.Error("reapFreshSessionLocked: s.registerStubByValue appears BEFORE `if stillExists {`; " +
			"the register must be inside the true branch of the still-exists guard (R090135-ARCH-2)")
	}
}
