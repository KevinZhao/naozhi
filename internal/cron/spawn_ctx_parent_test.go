package cron

import (
	"strings"
	"testing"
)

// TestSpawnCtxParentedOnStopCtx is the spawn-phase counterpart to
// TestSendCtxParentedOnStopCtx. R242-PERF-14 (#680) historically flagged
// `WithTimeout(context.Background(), jobTimeout)` for the spawn ctx
// (scheduler.go:2411 in the pre-split layout); the file split landed
// the call at scheduler_run.go:845 and rewrote the parent to s.stopCtx,
// so Stop() short-circuits an in-flight GetOrCreate the same way it
// short-circuits Send. Without a structural pin, a future refactor
// could silently re-introduce the Background parent — both restoring
// the historical use-after-free class race AND making the timer-heap
// pressure described in #680 unbounded by Stop deadlines (timers
// rooted in Background outlive every cancel scope).
//
// This is a small counterpart to TestSendCtxParentedOnStopCtx —
// duplication is acceptable because the two ctxs cancel independently
// (different lifecycles) and a single test asserting both fail-fast
// scenarios would have to thread two assertions per match path.
func TestSpawnCtxParentedOnStopCtx(t *testing.T) {
	t.Parallel()
	src := readSchedulerRunSource(t)

	// The exact line the issue points at. Both halves checked separately
	// so a reformat that wraps the call across a newline still passes.
	if !strings.Contains(src, "ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)") {
		t.Errorf("scheduler_run.go must parent spawnCtx on s.stopCtx;\n" +
			"R242-PERF-14 (#680): Background() parent leaks the spawn timer past Stop() " +
			"and reintroduces the use-after-free class race fixed in #1078")
	}

	// Negative anchor: the historical Background parent must NOT
	// reappear anywhere in the spawn declaration — a reviewer that
	// adds back `context.WithTimeout(context.Background(), jobTimeout)`
	// for either spawn or send fails this AND TestSendCtxParentedOnStopCtx.
	if strings.Contains(src, "spawnCancel := context.WithTimeout(context.Background()") {
		t.Errorf("scheduler_run.go still uses context.Background() for spawnCtx; " +
			"R242-PERF-14 / #680 regression — must be s.stopCtx")
	}
}

// TestSpawnCancelEarlyDocumented re-pins the R250-GO-15 (#1078) eager-
// cancel comment that explains why spawnCancel runs at GetOrCreate exit
// rather than relying on the function-end defer. The comment is the
// design rationale for the timer-heap pressure cited in #680: even with
// stopCtx parenting, a 500-job deployment running a slow Send phase
// would otherwise pin 500 spawn timers for the full Send window. The
// explicit eager cancel drops each timer the moment GetOrCreate
// returns, so timer-heap occupancy tracks "currently in spawn phase",
// not "currently executing".
//
// If a future refactor strips the rationale comment, this test fails
// and forces a conscious re-review against the timer-heap concern.
func TestSpawnCancelEarlyDocumented(t *testing.T) {
	t.Parallel()
	src := readSchedulerRunSource(t)
	for _, anchor := range []string{
		"R250-GO-15",          // anchor for the eager-cancel commit
		"timer",               // timer-heap rationale
		"defer spawnCancel()", // the bottom-of-function safety net
	} {
		if !strings.Contains(src, anchor) {
			t.Errorf("scheduler_run.go missing spawnCtx rationale anchor %q;\n"+
				"R242-PERF-14 (#680) timer-heap design hinges on the eager-cancel + defer pair.\n"+
				"Do NOT strip the comment without re-reviewing against the 500-job worst case",
				anchor)
		}
	}
}
