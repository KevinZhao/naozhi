package session

import (
	"context"
	"testing"
	"time"
)

// TestShutdown_CancelBeforeWait_KeepsShutdownFast pins shutdown()'s order:
// r.hist.cancel() must run before r.hist.wg.Wait(). It gives the order an
// in-flight load to observe: a task that HOLDS historyWg and only releases when
// historyCtx fires.
//
// With the documented order, cancel releases the task and the bounded Wait returns
// at once. Reversed, Wait blocks on a task that nothing has told to stop, so
// Shutdown pays the full 5s ceiling. The duration is the assertion.
func TestShutdown_CancelBeforeWait_KeepsShutdownFast(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.hist.ctx, r.hist.cancel = context.WithCancel(context.Background())

	// An in-flight history load: holds historyWg, parks on historyCtx.
	started := make(chan struct{})
	r.hist.wg.Add(1)
	go func() {
		defer r.hist.wg.Done()
		close(started)
		<-r.hist.ctx.Done()
	}()
	<-started

	start := time.Now()
	r.Shutdown()
	elapsed := time.Since(start)

	// The 5s ceiling is shutdown()'s bounded wait. Anything approaching it means
	// Wait ran before the cancel that releases the task.
	if elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v with one in-flight history load; historyCancel() must run BEFORE historyWg.Wait(), or the bounded wait parks on a task nothing has cancelled and every Shutdown on that path pays the 5s ceiling (R172-ARCH-D11)", elapsed)
	}
	// Premise: the task must actually have been released, or a fast Shutdown would
	// mean the wait was skipped rather than satisfied.
	select {
	case <-r.hist.ctx.Done():
	default:
		t.Error("historyCtx was never cancelled; Shutdown returned without releasing the in-flight load")
	}
}
