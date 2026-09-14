package session

import (
	"context"
	"testing"
	"time"
)

// TestShutdown_HistoryCtxCancelledFirst pins the R172-ARCH-D11 contract:
// Shutdown must cancel historyCtx as its first observable action so that
// in-flight LoadHistory*Ctx calls abort before the bounded historyWg.Wait
// even has a chance to start. The godoc for Router.shutdown() asserts this
// as the first step ("Cancel the history ctx ... BEFORE the bounded wait"),
// but that claim was only enforced by a comment. A future refactor could
// silently reorder the cancel below the Wait, re-introducing the exact hang
// the ctx was designed to short-circuit.
//
// Behaviour test: start a Router, park a goroutine on historyCtx.Done, call
// Shutdown, and assert the goroutine unblocks promptly — specifically before
// Shutdown returns. If Shutdown swapped the order (Wait first, cancel later),
// historyCtx would only be cancelled at the end and this test would either
// hang past its deadline or observe Shutdown completing before the ctx fires.
func TestShutdown_HistoryCtxCancelledFirst(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	// Sanity: the router was constructed without NewRouter, so historyCtx
	// was never wired. Supply one so the test exercises the real cancel path
	// rather than the nil-guard short-circuit.
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())

	// Observe historyCtx from a witness goroutine. It must fire before
	// Shutdown returns. We synchronise via a channel so the test does not
	// depend on Go's scheduler fairness.
	ctxDone := make(chan struct{})
	go func() {
		<-r.historyCtx.Done()
		close(ctxDone)
	}()

	// Sanity: before Shutdown, the ctx is live.
	select {
	case <-r.historyCtx.Done():
		t.Fatal("historyCtx fired before Shutdown was called")
	case <-time.After(20 * time.Millisecond):
	}

	// Trigger Shutdown and capture when it returns. The ctx must fire
	// strictly before Shutdown exits because the bounded wait sits after
	// the cancel.
	shutdownReturned := make(chan struct{})
	go func() {
		r.Shutdown()
		close(shutdownReturned)
	}()

	// historyCtx must fire within a generous budget — cancel is an
	// atomic store + goroutine wakeup, so 5s is practically "immediate"
	// while staying far from the 30s ShutdownTimeout.
	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("historyCtx was NOT cancelled within 5s of Shutdown starting — " +
			"the R172-ARCH-D11 ordering contract was broken. shutdown() must " +
			"call r.historyCancel() BEFORE blocking on historyWg.Wait().")
	}

	// Let Shutdown finish (it will; no running sessions) with a hard cap.
	select {
	case <-shutdownReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return within 3s after historyCtx fired — " +
			"unexpected second source of hangs.")
	}
}

// TestShutdown_CancelBeforeWait_KeepsShutdownFast is the behavioural replacement
// for TestShutdown_HistoryCtxCancelPreceedsHistoryWgWait, whose regexp asserted
// that r.historyCancel() appears before r.historyWg.Wait() in shutdown()'s source
// (Epic I #2547).
//
// That anchor's own comment said the sibling test above "catches a regression that
// actually hangs" while it caught "the more subtle case where a reorder happens to
// still work because there are no in-flight history loads to observe". Correct —
// and the fix is to give it one to observe: a task that HOLDS historyWg and only
// releases when historyCtx fires.
//
// With the documented order, cancel releases the task and the bounded Wait returns
// at once. Reversed, Wait blocks on a task that nothing has told to stop, so
// Shutdown pays the full 5s ceiling. The duration is the assertion.
func TestShutdown_CancelBeforeWait_KeepsShutdownFast(t *testing.T) {
	t.Parallel()
	r := newTestRouter(3)
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())

	// An in-flight history load: holds historyWg, parks on historyCtx.
	started := make(chan struct{})
	r.historyWg.Add(1)
	go func() {
		defer r.historyWg.Done()
		close(started)
		<-r.historyCtx.Done()
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
	case <-r.historyCtx.Done():
	default:
		t.Error("historyCtx was never cancelled; Shutdown returned without releasing the in-flight load")
	}
}
