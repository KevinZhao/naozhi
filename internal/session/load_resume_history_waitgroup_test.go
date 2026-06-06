package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// stubHistoryLoader returns a fixed batch so loadResumeHistoryOnSpawn's load
// path is exercised without touching the filesystem.
type stubHistoryLoader struct {
	entries []cli.EventEntry
	called  chan struct{}
}

func (l stubHistoryLoader) LoadHistoryChainTail(_ context.Context, _ string, _ []string, _ string, _ int) []cli.EventEntry {
	if l.called != nil {
		close(l.called)
	}
	return l.entries
}

// TestLoadResumeHistoryOnSpawn_CancelledPathDoesNotTouchWaitGroup pins the
// #1813 fix (mirror of #1655 for runHistoryTask): when historyCtx is already
// cancelled, loadResumeHistoryOnSpawn must check Err() BEFORE historyWg.Add(1).
// Under the old "Add(1) then compensate with Done()" shape, a late Add at
// counter==0 racing Shutdown's already-returned Wait() panics with "WaitGroup
// is reused before previous Wait has returned". We reproduce that hazard by
// draining the WaitGroup first, then invoking the loader on a cancelled ctx.
func TestLoadResumeHistoryOnSpawn_CancelledPathDoesNotTouchWaitGroup(t *testing.T) {
	// Reproduce Shutdown's exact hazard: historyCancel() fires, then a
	// detached historyWg.Wait() runs (router_cleanup.go) while an in-flight
	// spawn goroutine still reaches loadResumeHistoryOnSpawn. Under the buggy
	// "Add(1) before the Err() check" ordering, that Add(1) races the
	// concurrent Wait() at counter==0 → panic "WaitGroup is reused before
	// previous Wait has returned" / "Add called concurrently with Wait". With
	// the fix (Err() checked first) the cancelled spawn is a pure no-op so the
	// Wait/Add never overlap. Many iterations make the window deterministic
	// under -race.
	for iter := 0; iter < 300; iter++ {
		r := &Router{claudeDir: "/tmp/does-not-matter"}
		r.historyCtx, r.historyCancel = context.WithCancel(context.Background())
		r.historyCancel() // Shutdown signalled before the spawn lands.

		panicCh := make(chan any, 2)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)

		// Detached Wait, like shutdown()'s `go r.historyWg.Wait()`.
		wg := &r.historyWg
		go func() {
			defer done.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicCh <- rec
				}
			}()
			<-start
			wg.Wait()
		}()

		// In-flight spawn racing the Wait.
		go func() {
			defer done.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicCh <- rec
				}
			}()
			<-start
			r.loadResumeHistoryOnSpawn(context.Background(), &ManagedSession{key: "k"}, "k", "resume-id", "/ws", nil, nil)
		}()

		close(start)
		done.Wait()

		select {
		case rec := <-panicCh:
			t.Fatalf("iter %d: WaitGroup Add raced concurrent Wait on cancelled path: %v", iter, rec)
		default:
			// No panic — the cancelled spawn was a no-op as required.
		}
	}
}

// TestLoadResumeHistoryOnSpawn_LivePathLoadsAndAccounts confirms the happy
// path still works after reordering: a live historyCtx loads the chain,
// injects it, and historyWg.Wait blocks until the (synchronous) load returns.
func TestLoadResumeHistoryOnSpawn_LivePathLoadsAndAccounts(t *testing.T) {
	called := make(chan struct{})
	r := &Router{
		claudeDir:     "/tmp/does-not-matter",
		historyLoader: stubHistoryLoader{entries: mkEntries("h", 3), called: called},
	}
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())
	defer r.historyCancel()

	s := &ManagedSession{key: "k"}
	r.loadResumeHistoryOnSpawn(context.Background(), s, "k", "resume-id", "/ws", nil, nil)

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("history loader was not invoked on the live path")
	}
	r.historyWg.Wait() // must not deadlock — Done deferred inside the IIFE

	if got := len(s.EventEntries()); got != 3 {
		t.Fatalf("live path injected %d entries, want 3", got)
	}
}

// TestLoadResumeHistoryOnSpawn_NoResumeIDIsNoOp guards the early-return
// preconditions so the WaitGroup is never touched when there's nothing to load.
func TestLoadResumeHistoryOnSpawn_NoResumeIDIsNoOp(t *testing.T) {
	r := &Router{claudeDir: "/tmp/x"}
	r.historyCtx, r.historyCancel = context.WithCancel(context.Background())
	defer r.historyCancel()
	r.historyWg.Wait()

	r.loadResumeHistoryOnSpawn(context.Background(), &ManagedSession{key: "k"}, "k", "", "/ws", nil, nil)
	r.historyWg.Wait() // zero counter, no block
}
