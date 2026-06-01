package cli

import (
	"context"
	"testing"
	"time"
)

// TestResolve_SemaphoreTimeout_ContextWithTimeout pins [R112714-PERF-10]:
// when the semaphore is full and the timeout elapses (not ctx cancel),
// Resolve must return (LinkInfo{}, false) promptly without deadlocking.
func TestResolve_SemaphoreTimeout_ContextWithTimeout(t *testing.T) {
	t.Parallel()
	const sessionID = "sem-timeout-test-uuid-cccccccccc"
	l, _ := newLinkerForTest(t, sessionID)
	// Short retry budget so the timeout fires quickly.
	l.retryInterval = 20 * time.Millisecond
	l.retryLimit = 2 // timeout = (2+1)*20ms = 60ms

	// Saturate resolveSem so no slot is available.
	for i := 0; i < cap(l.resolveSem); i++ {
		l.resolveSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(l.resolveSem); i++ {
			<-l.resolveSem
		}
	}()

	start := time.Now()
	info, resolved := l.Resolve(context.Background(), "t_semtimeout", "toolu_T", "missing-sem", "", time.Now().UnixMilli())
	elapsed := time.Since(start)

	// Should return within ~3x the timeout budget (scheduler slack).
	maxBudget := time.Duration(l.retryLimit+1)*l.retryInterval*3 + 100*time.Millisecond
	if elapsed > maxBudget {
		t.Fatalf("semaphore timeout must abort within %s, elapsed=%s", maxBudget, elapsed)
	}
	if resolved {
		t.Error("Resolve must return resolved=false on semaphore timeout")
	}
	if info.InternalAgentID != "" {
		t.Errorf("Resolve must return empty LinkInfo on semaphore timeout, got %+v", info)
	}
}
