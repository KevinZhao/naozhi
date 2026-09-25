package subagent

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestResolve_SemaphoreTimeout_ContextWithTimeout pins [R112714-PERF-10]:
// when the semaphore is full and the timeout elapses (not ctx cancel),
// Resolve must return (LinkInfo{}, false) promptly without deadlocking.
//
// The property is "bounded by the semaphore budget, not blocked forever", and
// it has two halves that need different mechanisms:
//
//   - Does not block forever — enforced by running Resolve in a goroutine
//     with a hard timeout. That is the regression this test exists for, and a
//     wall-clock assertion is the wrong tool for it.
//   - Actually waits the budget — enforced by a lower bound. Without it the
//     test was half vacuous: setting semTimeout to 0 (i.e. giving up on the
//     semaphore without waiting at all, a real behaviour regression) left the
//     old assertions passing.
func TestResolve_SemaphoreTimeout_ContextWithTimeout(t *testing.T) {
	t.Parallel()
	const sessionID = "sem-timeout-test-uuid-cccccccccc"
	l, _ := newLinkerForTest(t, sessionID)
	// Budget = (0+1)*50ms. Kept small because the ceiling below is relative
	// to it, so precision does not come from a long wait.
	l.retryInterval = 50 * time.Millisecond
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

	// Upper bound: the wait must not overrun its own timeout by a wide
	// margin. Expressed relative to the budget (not as absolute ms) so it
	// scales with the constants above; the old absolute 280ms ceiling was
	// the kind of bound that fails on a loaded runner while the code is fine.
	// 4x leaves room for scheduling slack on the shared CI runners.
	if maxBudget := semBudget * 4; res.elapsed > maxBudget {
		t.Errorf("semaphore timeout must abort within %s (4x the %s budget), elapsed=%s",
			maxBudget, semBudget, res.elapsed)
	}
	// Lower bound: Resolve must actually have waited out the budget, not
	// bailed early — see the doc comment. Go timers never fire early, so a
	// small tolerance is enough and this cannot flake on a slow machine.
	if minBudget := semBudget - 10*time.Millisecond; res.elapsed < minBudget {
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

// TestResolve_FreeSlotIsTakenEvenWhenTheBudgetIsSpent: with a slot free,
// Resolve must take it. The wait for a slot is a select between the send and
// the budget's timer, and select picks at random among ready cases, so a
// budget that had already run out by the time select ran (a zero budget
// here; a goroutine preempted past a short one on a loaded machine) dropped
// the call about half the time despite the idle semaphore. That made
// TestAgentEvents_Tombstone_404 flaky (#2746).
func TestResolve_FreeSlotIsTakenEvenWhenTheBudgetIsSpent(t *testing.T) {
	t.Parallel()
	const sessionID = "sem-free-slot-test-uuid-dddddddd"
	l, _ := newLinkerForTest(t, sessionID)
	l.retryInterval = 0 // budget (0+1)*0: spent before select runs
	l.retryLimit = 0

	for i := 0; i < 200; i++ {
		taskID := fmt.Sprintf("t_free_%03d", i)
		info, resolved := l.Resolve(context.Background(), taskID, "toolu_F", "missing", "", time.Now().UnixMilli())
		if !resolved || !info.Resolved {
			t.Fatalf("call %d: Resolve dropped with the semaphore idle (resolved=%v info=%+v)", i, resolved, info)
		}
	}
	if n := len(l.resolveSem); n != 0 {
		t.Errorf("%d slots still held after every call returned", n)
	}
}
