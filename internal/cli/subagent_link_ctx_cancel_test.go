package cli

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestR218BGO3_ResolveRespectsCtxCancelDuringRetrySleep pins the issue
// #644 contract: when ctx is canceled while Resolve is sleeping in the
// retry loop, Resolve must return promptly (~retryInterval) instead of
// running the full retryLimit*retryInterval budget. Otherwise process
// shutdown (which closes p.done → cancels the lifecycle ctx) is delayed
// up to 3s for every parked Resolve goroutine.
func TestR218BGO3_ResolveRespectsCtxCancelDuringRetrySleep(t *testing.T) {
	t.Parallel()
	const sessionID = "ctx-cancel-test-uuid-aaaaaaaaaaa"
	l, _ := newLinkerForTest(t, sessionID)
	// Stretch the retry budget so the bug, if reintroduced, would be
	// trivially observable (12 * 100ms = 1.2s vs the single-tick
	// ≤ 100ms cancel budget the fix should observe).
	l.retryInterval = 100 * time.Millisecond
	l.retryLimit = 12

	ctx, cancel := context.WithCancel(context.Background())

	// No agent files exist for this name → Resolve enters the retry loop
	// and hits the time.Sleep / sleepOrCancel branch.
	const taskID = "tcancel"
	const name = "missing-name"

	// Cancel ctx after one retryInterval so we hit sleepOrCancel mid-wait.
	go func() {
		time.Sleep(l.retryInterval / 2)
		cancel()
	}()

	start := time.Now()
	info, resolved := l.Resolve(ctx, taskID, "toolu_X", name, "", time.Now().UnixMilli())
	elapsed := time.Since(start)

	// Cancel must short-circuit Resolve well below the full retry budget.
	// Allow 2 retryIntervals (1 tick to observe, 1 slack for scheduling).
	maxBudget := 3 * l.retryInterval
	if elapsed > maxBudget {
		t.Fatalf("ctx cancel must abort Resolve within %s, elapsed=%s", maxBudget, elapsed)
	}
	// On cancel, Resolve returns the empty zero LinkInfo and resolved=false
	// so the next reattach can try again rather than caching a tombstone
	// for a spurious cancel.
	if resolved {
		t.Errorf("Resolve must return resolved=false when ctx cancels, got info=%+v", info)
	}
	if info.InternalAgentID != "" || info.JSONLPath != "" {
		t.Errorf("canceled Resolve must not write a partial LinkInfo, got %+v", info)
	}
}

// TestR218BGO3_ResolveRespectsCtxCancelOnSemaphoreAcquire pins the
// resolveSem half of the contract: when the semaphore is full and ctx
// cancels before a slot frees, Resolve must return promptly. Without
// the ctx.Done() arm in the sem select, a shutdown would wait the full
// (retryLimit+1)*retryInterval timer budget before unblocking.
func TestR218BGO3_ResolveRespectsCtxCancelOnSemaphoreAcquire(t *testing.T) {
	t.Parallel()
	const sessionID = "sem-cancel-test-uuid-bbbbbbbbbbb"
	l, _ := newLinkerForTest(t, sessionID)
	// Long retry budget so the timer arm is far away — only the ctx.Done
	// arm should fire promptly.
	l.retryInterval = 200 * time.Millisecond
	l.retryLimit = 12

	// Saturate resolveSem (capacity = maxConcurrentResolves) by writing
	// directly so no slot is available.
	for i := 0; i < cap(l.resolveSem); i++ {
		l.resolveSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(l.resolveSem); i++ {
			<-l.resolveSem
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	var elapsed time.Duration
	var resolved bool
	go func() {
		defer wg.Done()
		start := time.Now()
		// Use a name that would also fail the fast-path lookup so we'd
		// reach the sem acquire arm.
		_, ok := l.Resolve(ctx, "tsem", "toolu_S", "missing", "", time.Now().UnixMilli())
		elapsed = time.Since(start)
		resolved = ok
	}()

	// Cancel after a tiny delay so Resolve is parked at the sem select.
	time.Sleep(20 * time.Millisecond)
	cancel()

	wg.Wait()

	// Cancel must abort the sem wait well below the retry-budget timeout.
	maxBudget := 500 * time.Millisecond
	if elapsed > maxBudget {
		t.Fatalf("ctx cancel must abort sem wait within %s, elapsed=%s", maxBudget, elapsed)
	}
	if resolved {
		t.Errorf("Resolve must return resolved=false when sem cancel fires")
	}
}
