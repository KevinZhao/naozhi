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

// TestSpawnCancelDeferSafetyNet pins the bottom-of-function `defer spawnCancel()`
// that backs the eager cancel at GetOrCreate exit (#1078).
func TestSpawnCancelDeferSafetyNet(t *testing.T) {
	t.Parallel()
	src := readSchedulerRunSource(t)
	if !strings.Contains(src, "defer spawnCancel()") {
		t.Error("scheduler_run.go missing `defer spawnCancel()` safety net (#1078)")
	}
}
