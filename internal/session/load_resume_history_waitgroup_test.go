package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// stubHistoryLoader returns a fixed batch so loadResumeHistoryOnSpawn's load
// path is exercised without touching the filesystem.
type stubHistoryLoader struct {
	entries []clievent.EventEntry
	called  chan struct{}
}

func (l stubHistoryLoader) LoadHistoryChainTail(_ context.Context, _ string, _ []string, _ string, _ int) []clievent.EventEntry {
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
		r := &Router{ss: newSessionTable(), hist: HistoryIO{claudeDir: "/tmp/does-not-matter"}}
		r.hist.ctx, r.hist.cancel = context.WithCancel(context.Background())
		r.hist.cancel() // Shutdown signalled before the spawn lands.

		panicCh := make(chan any, 2)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)

		// Detached Wait, like shutdown()'s `go r.hist.wg.Wait()`.
		wg := &r.hist.wg
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
			r.hist.loadResumeHistoryOnSpawn(context.Background(), &ManagedSession{key: "k"}, "k", "resume-id", "/ws", nil, nil)
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

// TestLoadResumeHistoryOnSpawn_CancelDuringSpawnNoPanic pins the residual
// TOCTOU the Err()-first ordering alone could not close (R202606b-GO-001,
// #2186): a cancel landing AFTER the nil-Err check but BEFORE historyWg.Add(1)
// would re-add to a WaitGroup already drained to 0 with a detached Wait in
// flight, panicking "WaitGroup is reused before previous Wait has returned".
//
// Unlike the cancelled-path test above, here historyCtx starts LIVE and the
// cancel races the spawn. The producer (loadResumeHistoryOnSpawn) and the
// shutdown-style cancel both take historyWgMu, so the check+Add is atomic vs
// the cancel and the detached Wait can never observe a transient +1 at
// counter 0. Many iterations under -race drive the interleave deterministically.
func TestLoadResumeHistoryOnSpawn_CancelDuringSpawnNoPanic(t *testing.T) {
	for iter := 0; iter < 300; iter++ {
		r := &Router{
			ss:   newSessionTable(),
			hist: HistoryIO{claudeDir: "/tmp/does-not-matter", loader: stubHistoryLoader{entries: mkEntries("h", 1)}},
		}
		r.hist.ctx, r.hist.cancel = context.WithCancel(context.Background())

		panicCh := make(chan any, 2)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)

		// Producer: in-flight resume spawn on a (initially) live ctx.
		go func() {
			defer done.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicCh <- rec
				}
			}()
			<-start
			r.hist.loadResumeHistoryOnSpawn(context.Background(), &ManagedSession{key: "k"}, "k", "resume-id", "/ws", nil, nil)
		}()

		// Shutdown side: the locked cancel shutdown() runs, then a detached Wait.
		go func() {
			defer done.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panicCh <- rec
				}
			}()
			<-start
			r.hist.cancelTasks()
			r.hist.wg.Wait()
		}()

		close(start)
		done.Wait()

		select {
		case rec := <-panicCh:
			t.Fatalf("iter %d: cancel raced check+Add → %v", iter, rec)
		default:
		}
	}
}

// TestLoadResumeHistoryOnSpawn_LivePathLoadsAndAccounts confirms the happy
// path still works after reordering: a live historyCtx loads the chain,
// injects it, and historyWg.Wait blocks until the (synchronous) load returns.
func TestLoadResumeHistoryOnSpawn_LivePathLoadsAndAccounts(t *testing.T) {
	called := make(chan struct{})
	r := &Router{
		ss:   newSessionTable(),
		hist: HistoryIO{claudeDir: "/tmp/does-not-matter", loader: stubHistoryLoader{entries: mkEntries("h", 3), called: called}},
	}
	r.hist.ctx, r.hist.cancel = context.WithCancel(context.Background())
	defer r.hist.cancel()

	s := &ManagedSession{key: "k"}
	r.hist.loadResumeHistoryOnSpawn(context.Background(), s, "k", "resume-id", "/ws", nil, nil)

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("history loader was not invoked on the live path")
	}
	r.hist.wg.Wait() // must not deadlock — Done deferred inside the IIFE

	if got := len(s.EventEntries()); got != 3 {
		t.Fatalf("live path injected %d entries, want 3", got)
	}
}

// TestLoadResumeHistoryOnSpawn_NoResumeIDIsNoOp guards the early-return
// preconditions so the WaitGroup is never touched when there's nothing to load.
func TestLoadResumeHistoryOnSpawn_NoResumeIDIsNoOp(t *testing.T) {
	r := &Router{ss: newSessionTable(), hist: HistoryIO{claudeDir: "/tmp/x"}}
	r.hist.ctx, r.hist.cancel = context.WithCancel(context.Background())
	defer r.hist.cancel()
	r.hist.wg.Wait()

	r.hist.loadResumeHistoryOnSpawn(context.Background(), &ManagedSession{key: "k"}, "k", "", "/ws", nil, nil)
	r.hist.wg.Wait() // zero counter, no block
}
