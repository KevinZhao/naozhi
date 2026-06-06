package session

// R217-ARCH-7 (#627) regression: NewRouter previously inlined the tier 1 /
// tier 2 history-load goroutine spawning blocks (~165 lines). They were
// extracted into startBackgroundHistoryLoaders so the constructor stays
// readable and the tier ordering / shared-semaphore contract has a
// single named home.
//
// The test below pins the structural invariant: a method named
// startBackgroundHistoryLoaders exists on *Router, is callable in the
// "no sessions / no eventLogDir / no claudeDir" zero-config state
// without panicking, spawns no goroutines (both tier guards short-
// circuit), and returns immediately. A future refactor that re-inlines
// the helper or renames it past this test will fail compile here.

import (
	"context"
	"testing"
)

// TestStartBackgroundHistoryLoaders_NoOpOnEmptyRouter exercises the
// extracted helper on a freshly-constructed Router with no sessions
// and none of the persistence wiring set up. The expected behaviour
// is a quiet return: no goroutines spawned (tier 1 gated on
// r.eventLogPersister != nil and tier 2 on r.claudeDir != ""), no
// panics, no historyWg work to wait for.
func TestStartBackgroundHistoryLoaders_NoOpOnEmptyRouter(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &Router{
		ss:         sessionStore{sessions: map[string]*ManagedSession{}},
		historyCtx: ctx,
	}

	// Direct call — compiles only if the helper exists. The
	// historyWg.Wait() below pins the "no goroutines spawned"
	// invariant: an accidentally-launched goroutine that blocks
	// indefinitely would deadlock this test.
	r.startBackgroundHistoryLoaders()
	r.historyWg.Wait()
}
