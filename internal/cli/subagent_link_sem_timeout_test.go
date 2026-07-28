package cli

import (
	"context"
	"testing"
	"time"
)

// TestResolve_SemaphoreTimeout_ContextWithTimeout pins [R112714-PERF-10]:
// when the semaphore is full and the timeout elapses (not ctx cancel),
// Resolve must return (LinkInfo{}, false) promptly without deadlocking.
//
// The property under test is "bounded by the semaphore budget, not blocked
// forever". Expressing that as an absolute millisecond ceiling made this test
// flaky on slow runners: with a 60ms budget the old assertion allowed 280ms
// total, but a loaded macOS CI runner needed ~363ms just for the scheduling
// around it, so the test failed while the code under test was correct.
//
// Two changes make the bound meaningful instead of tight:
//   - A deliberately LARGE retry budget (1s), so real scheduling slack is a
//     small fraction of it rather than a multiple.
//   - The ceiling is expressed relative to that budget, and the "did not
//     deadlock" half is enforced by running Resolve in a goroutine with a
//     hard timeout — which is the actual regression this pins.
func TestResolve_SemaphoreTimeout_ContextWithTimeout(t *testing.T) {
	t.Parallel()
	const sessionID = "sem-timeout-test-uuid-cccccccccc"
	l, _ := newLinkerForTest(t, sessionID)
	// Budget large enough that scheduler jitter cannot rival it, yet small
	// enough to keep the test fast: (0+1)*1s = 1s.
	l.retryInterval = time.Second
	l.retryLimit = 0
	semBudget := time.Duration(l.retryLimit+1) * l.retryInterval

	// Saturate resolveSem so no slot is available.
	for i := 0; i < cap(l.resolveSem); i++ {
		l.resolveSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(l.resolveSem); i++ {
			<-l.resolveSem
		}
	}()

	type result struct {
		info     LinkInfo
		resolved bool
		elapsed  time.Duration
	}
	resCh := make(chan result, 1)
	go func() {
		start := time.Now()
		info, resolved := l.Resolve(context.Background(), "t_semtimeout", "toolu_T", "missing-sem", "", time.Now().UnixMilli())
		resCh <- result{info: info, resolved: resolved, elapsed: time.Since(start)}
	}()

	// Deadlock guard: generous enough that only a genuinely stuck Resolve
	// trips it, so this arm never fires on a merely slow machine.
	var res result
	select {
	case res = <-resCh:
	case <-time.After(semBudget + 30*time.Second):
		t.Fatalf("Resolve did not return within %s of the %s semaphore budget — likely deadlocked",
			30*time.Second, semBudget)
	}

	// Upper bound relative to the budget: the semaphore wait must not be
	// retried or compounded. 4x absorbs any realistic CI scheduling slack
	// while still catching a regression that, say, waits the full retry loop
	// per attempt.
	if maxBudget := semBudget * 4; res.elapsed > maxBudget {
		t.Errorf("semaphore timeout must abort within %s (4x the %s budget), elapsed=%s",
			maxBudget, semBudget, res.elapsed)
	}
	// Lower bound: it must actually have waited for the budget rather than
	// bailing immediately for some unrelated reason, which would make the
	// upper bound vacuous. Allow a small timer-granularity shortfall.
	if minBudget := semBudget - 50*time.Millisecond; res.elapsed < minBudget {
		t.Errorf("Resolve returned after %s, before the %s semaphore budget elapsed — "+
			"the timeout path may not be what aborted it", res.elapsed, semBudget)
	}
	if res.resolved {
		t.Error("Resolve must return resolved=false on semaphore timeout")
	}
	if res.info.InternalAgentID != "" {
		t.Errorf("Resolve must return empty LinkInfo on semaphore timeout, got %+v", res.info)
	}
}
