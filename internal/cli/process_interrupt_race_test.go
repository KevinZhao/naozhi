package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// R39-CONCUR1 — drainStaleEvents must clear interrupted/interruptedRun
// atomically w.r.t. Interrupt() / InterruptViaControl(). The pre-fix code
// ran two naked Swap(false) calls; a concurrent Interrupt() landing between
// the two Swaps could lose its signal (interruptedRun Swap'd to false while
// the caller observed wasRunning=false, causing the next Send's drain to
// skip the settle window and leak the SIGINT-produced result event into the
// following turn).
//
// These tests pin both the behavioural expectations and the source-level
// locking contract so a future refactor cannot silently reopen the race.

// --- behavioural regressions -----------------------------------------------

// Pre-fix, this sequence of flag reads would skip the settle wait entirely:
//  1. drainStaleEvents calls interrupted.Swap(false)   → true (consumed)
//  2. Interrupt() (racing) calls Store(true, true)     → flags now (true,true)
//  3. drainStaleEvents calls interruptedRun.Swap(false) → true (consumed)
//  4. The next Send's drain sees (false,false) → no settle wait → leak.
//
// We cannot reproduce the race deterministically without instrumenting atomic
// operations, so we exercise the fix's invariant directly: once
// drainStaleEvents returns with wasInterrupted observed true, any Interrupt()
// that lands concurrently must either be fully applied (caught by this drain)
// or fully deferred (caught by the next drain) — never "half consumed".
func TestProcess_DrainStaleEvents_ConcurrentInterruptLossless(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("stress loop; skipped under -short")
	}
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	// Let readLoop reach steady-state before we start racing atomics.
	time.Sleep(10 * time.Millisecond)

	const iterations = 5000
	var lostSignals atomic.Int64

	// Producer: repeatedly mark (interrupted, interruptedRun) = (true, true)
	// under p.mu, mirroring Interrupt()'s contract in the State==Running
	// branch. Under the pre-fix two-Swap code, this is the exact pattern
	// that could leak: drain consumes interrupted from a prior Interrupt,
	// producer re-Stores(true,true), drain consumes interruptedRun — and
	// the producer's fresh Interrupt bits are silently lost.
	var producerWG sync.WaitGroup
	stopProducer := make(chan struct{})
	producerWG.Add(1)
	go func() {
		defer producerWG.Done()
		for {
			select {
			case <-stopProducer:
				return
			default:
			}
			p.mu.Lock()
			p.interrupted.Store(true)
			p.interruptedRun.Store(true)
			p.mu.Unlock()
			runtime.Gosched()
		}
	}()

	// Feeder: drain's settle window blocks on a result event. Feed a
	// steady stream so every drain that enters settle returns fast.
	var feederWG sync.WaitGroup
	stopFeeder := make(chan struct{})
	feederWG.Add(1)
	go func() {
		defer feederWG.Done()
		ticker := time.NewTicker(100 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFeeder:
				return
			case <-ticker.C:
				srv.SendStdout(`{"type":"result","result":"race_drain"}`)
			}
		}
	}()

	// Consumer: after each drain, check the "half-consumed" invariant.
	// The pre-fix code could leave (interrupted=false, interruptedRun=true)
	// when a producer Stored(true,true) between drain's two Swaps. With the
	// fix, both Swaps happen under p.mu — a concurrent producer is
	// serialised either entirely before or entirely after, never interleaved.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < iterations; i++ {
		_ = p.drainStaleEvents(ctx)
		p.mu.Lock()
		interrupted := p.interrupted.Load()
		running := p.interruptedRun.Load()
		p.mu.Unlock()
		if !interrupted && running {
			lostSignals.Add(1)
		}
	}
	close(stopProducer)
	close(stopFeeder)
	producerWG.Wait()
	feederWG.Wait()

	// Final quiescent check: drain once more while no producer is racing.
	// Must leave both flags clear.
	if err := p.drainStaleEvents(context.Background()); err != nil {
		t.Errorf("final drainStaleEvents() err = %v", err)
	}
	if p.interrupted.Load() || p.interruptedRun.Load() {
		t.Errorf("after quiescent drain: interrupted=%v run=%v, want both false",
			p.interrupted.Load(), p.interruptedRun.Load())
	}
	if n := lostSignals.Load(); n > 0 {
		t.Errorf("observed %d lost-signal windows (interrupted=false, interruptedRun=true); "+
			"the two Swap(false) calls in drainStaleEvents are not atomic w.r.t. Interrupt()", n)
	}
}

// Pin that drain clears BOTH flags even when the pre-entry state is the
// "impossible" (false, true) — older code gated interruptedRun.Swap behind
// interrupted.Swap returning true, so (false, true) would persist forever.
// Post-fix, drain unconditionally Swaps both atomics (under mu) so no stale
// bit can survive.
func TestProcess_DrainStaleEvents_ClearsBothFlagsRegardlessOfEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		interrupted       bool
		interruptedRun    bool
		wantInterrupted   bool
		wantInterruptedRn bool
	}{
		{"both_clear", false, false, false, false},
		{"interrupted_only", true, false, false, false},
		{"both_set", true, true, false, false},
		// This (false, true) state should not arise during normal operation
		// (Interrupt always sets interrupted=true before checking Running),
		// but the fix means drain still normalises it rather than leaving
		// a dangling bit that the next Send would honour.
		{"leaked_run_only", false, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := shimTestPair(&ClaudeProtocol{})
			startServerDrain(srv)
			p.startReadLoop()
			defer p.Kill()

			p.interrupted.Store(tc.interrupted)
			p.interruptedRun.Store(tc.interruptedRun)

			// When (true, true), drain enters the settle window and waits
			// for a result event or timer. Feed it a synthetic result so
			// the test does not pay 500ms.
			if tc.interrupted && tc.interruptedRun {
				go func() {
					time.Sleep(20 * time.Millisecond)
					srv.SendStdout(`{"type":"result","result":"drain"}`)
				}()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := p.drainStaleEvents(ctx); err != nil {
				t.Fatalf("drainStaleEvents() err = %v", err)
			}
			if got := p.interrupted.Load(); got != tc.wantInterrupted {
				t.Errorf("interrupted = %v, want %v", got, tc.wantInterrupted)
			}
			if got := p.interruptedRun.Load(); got != tc.wantInterruptedRn {
				t.Errorf("interruptedRun = %v, want %v", got, tc.wantInterruptedRn)
			}
		})
	}
}

// --- source-level contract -------------------------------------------------

// TestDrainStaleEvents_SwapCallsAreUnderMu pins the fix at the source level:
// both interrupted.Swap(false) and interruptedRun.Swap(false) must appear
// inside drainStaleEvents between a p.mu.Lock()/Unlock() pair. If a future
// refactor moves either Swap outside the lock, the R39-CONCUR1 race reopens
// and no behavioural test will reliably catch it — hence this static check.
func TestDrainStaleEvents_SwapCallsAreUnderMu(t *testing.T) {
	t.Parallel()
	// Locate process.go relative to this test file.
	_, thisFile, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(thisFile), "process.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read process.go: %v", err)
	}
	body := string(data)

	// Extract the drainStaleEvents function body. The marker prefix is
	// stable — it is the function's signature; the body ends at the next
	// top-level `// ---` or `func (` line. We parse naively: find the
	// signature, then slice until the next unindented `}` that closes the
	// function. Since Go formatter keeps closing braces column-0, this
	// heuristic is reliable without a full parser.
	const sig = "func (p *Process) drainStaleEvents(ctx context.Context) error {"
	start := strings.Index(body, sig)
	if start < 0 {
		t.Fatalf("could not find drainStaleEvents signature in process.go")
	}
	rest := body[start:]
	// Find the next line that begins with `}` (column 0) — that closes the fn.
	end := -1
	for idx := 0; idx < len(rest); {
		nl := strings.Index(rest[idx:], "\n}")
		if nl < 0 {
			break
		}
		// Accept only if the char after '}' is '\n' or EOF (i.e. '}' alone on a line).
		candEnd := idx + nl + 2
		if candEnd >= len(rest) || rest[candEnd-1] == '\n' || rest[candEnd] == '\n' {
			end = candEnd
			break
		}
		idx += nl + 1
	}
	if end < 0 {
		t.Fatalf("could not locate end of drainStaleEvents body")
	}
	fnBody := rest[:end]

	// Find the position of both Swap(false) calls and verify each sits
	// between a `p.mu.Lock()` and a matching `p.mu.Unlock()` call that
	// appear earlier and later respectively in the function body.
	type swap struct {
		field string
		at    int
	}
	var swaps []swap
	for _, f := range []string{"p.interrupted.Swap(false)", "p.interruptedRun.Swap(false)"} {
		pos := strings.Index(fnBody, f)
		if pos < 0 {
			t.Errorf("drainStaleEvents: %q not found — the fix may have been reverted or refactored; "+
				"update this test to track the new clear-path", f)
			continue
		}
		swaps = append(swaps, swap{field: f, at: pos})
	}
	if t.Failed() {
		return
	}

	// Find the nearest enclosing Lock/Unlock pair.
	lockIdx := strings.Index(fnBody, "p.mu.Lock()")
	unlockIdx := strings.Index(fnBody, "p.mu.Unlock()")
	if lockIdx < 0 || unlockIdx < 0 {
		t.Fatalf("drainStaleEvents must acquire p.mu around the Swap calls to keep the "+
			"Interrupt()/drain read path symmetric (R39-CONCUR1). Found Lock=%d Unlock=%d",
			lockIdx, unlockIdx)
	}
	if unlockIdx < lockIdx {
		t.Fatalf("drainStaleEvents: Unlock precedes Lock — malformed critical section")
	}
	for _, s := range swaps {
		if s.at < lockIdx || s.at > unlockIdx {
			t.Errorf("drainStaleEvents: %q is outside the p.mu critical section "+
				"(lock at %d, unlock at %d, swap at %d). R39-CONCUR1 requires both Swap "+
				"calls under p.mu so concurrent Interrupt() Store sequences (which "+
				"run under p.mu per contract on lines 995-1001) cannot interleave.",
				s.field, lockIdx, unlockIdx, s.at)
		}
	}
}
