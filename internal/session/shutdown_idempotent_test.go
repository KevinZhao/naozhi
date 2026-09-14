package session

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// shutdown_idempotent_test.go — the single-shot contract, observed rather than
// grepped. Epic I #2547.
//
// TestShutdown_SingleShotContract read router_core.go and router_cleanup.go for
// three strings: a `shutdownOnce sync.Once` field, `r.shutdownOnce.Do(r.shutdown)`
// as Shutdown's body, and the comment "intentionally left running".
//
// The first two are the same invariant — Shutdown runs its body once — and that
// is observable: call it repeatedly and count. The third is a comment check,
// which CLAUDE.md's own rule rules out ("do not add new lints that validate
// comments", and a source-scanning test is the same tax with a different label).
// The comment is worth keeping, but a test is not what keeps it: the godoc on
// Shutdown and the reasoning in this file's header are.
//
// Verified that these catch what the strings could not: replacing the Once gate
// with a plain bool keeps the `shutdownOnce sync.Once` FIELD in place (so check 1
// passes) while introducing a data race between concurrent callers — reported as
// DATA RACE by TestShutdown_ConcurrentCallersRunTheBodyOnce. Replacing it with
// direct dispatch gives "body ran 6 times, want 1".
//
// R44-REL-HIST-GOROUTINE: shutdown() leaks the wrapper goroutine around
// historyWg.Wait() when the 5s bounded wait expires. That is acceptable ONLY
// because the body runs once per Router. A reusable Shutdown accumulates one
// orphan per timed-out cycle.
func TestShutdown_RunsItsBodyOnce(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{MaxProcs: 1})

	// historyCancel is the first thing shutdown() touches, and it is nil-checked,
	// so counting its invocations counts body entries without needing a hook in
	// the middle of teardown.
	var calls int
	r.historyCancel = func() { calls++ }

	r.Shutdown()
	if calls != 1 {
		t.Fatalf("after one Shutdown, body ran %d times, want 1", calls)
	}
	for range 5 {
		r.Shutdown()
	}
	if calls != 1 {
		t.Errorf("after six Shutdowns, body ran %d times, want 1 — a reusable Shutdown accumulates one orphan goroutine per cycle that times out on hung I/O (R44-REL-HIST-GOROUTINE)", calls)
	}
}

// TestShutdown_ConcurrentCallersRunTheBodyOnce is the other half of what the
// Once gate buys: R49-REL-SHUTDOWN-ONCE was a race between broadcast-timer
// teardown and a second caller re-entering. Serial idempotency alone would be
// satisfied by a plain bool flag, which that race is about.
func TestShutdown_ConcurrentCallersRunTheBodyOnce(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{MaxProcs: 1})

	var mu sync.Mutex
	calls := 0
	r.historyCancel = func() {
		mu.Lock()
		calls++
		mu.Unlock()
	}

	const callers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.Shutdown()
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d concurrent Shutdown calls ran the body %d times, want 1", callers, got)
	}
}

// TestShutdown_DoesNotLeakPerCall bounds the goroutine cost of repeated calls.
// The wrapper goroutine around historyWg.Wait() exits as soon as the wait
// returns, which with no history tasks is immediate; the point here is that N
// calls do not produce N of them.
func TestShutdown_DoesNotLeakPerCall(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 1})
	before := runtime.NumGoroutine()
	for range 20 {
		r.Shutdown()
	}
	// Give the single wrapper goroutine a moment to finish.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Errorf("goroutines %d -> %d after 20 Shutdown calls; the single-shot gate is not holding", before, got)
	}
}
