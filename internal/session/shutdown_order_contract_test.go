package session

import (
	"context"
	"os"
	"regexp"
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

// TestShutdown_HistoryCtxCancelPreceedsHistoryWgWait is a source-level pin
// on the instruction order within shutdown(). A behaviour test (above)
// catches a regression that actually hangs; this one catches the more
// subtle case where a reorder happens to still "work" because there are no
// in-flight history loads to observe. The two are complementary.
//
// Contract: the first mutating statement inside shutdown() must be the
// `r.historyCancel()` call, followed (eventually) by the
// `r.historyWg.Wait()` call inside a goroutine. Reordering these two is
// the failure mode R172-ARCH-D11 calls out explicitly.
func TestShutdown_HistoryCtxCancelPreceedsHistoryWgWait(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}

	// Locate the shutdown() function body. We tolerate any leading
	// godoc and any amount of interior whitespace — the only invariant
	// we care about is "historyCancel() appears before historyWg.Wait()".
	reShutdown := regexp.MustCompile(`(?ms)^func \(r \*Router\) shutdown\(\) \{(.*?)\n\}\n`)
	m := reShutdown.FindSubmatch(src)
	if m == nil {
		t.Fatal("could not locate `func (r *Router) shutdown()` body in router.go — " +
			"did the signature change? Update this test to find the new anchor.")
	}
	body := m[1]

	reCancel := regexp.MustCompile(`r\.historyCancel\(\)`)
	reWait := regexp.MustCompile(`r\.historyWg\.Wait\(\)`)

	cancelIdx := reCancel.FindIndex(body)
	waitIdx := reWait.FindIndex(body)

	if cancelIdx == nil {
		t.Fatal("shutdown() body no longer contains `r.historyCancel()`. " +
			"If you moved history-ctx cancellation out of Shutdown, audit callers " +
			"of r.historyCtx (LoadHistory*Ctx, deferred JSONL backfill) — they " +
			"will now only abort at process-teardown, not on normal Shutdown.")
	}
	if waitIdx == nil {
		t.Fatal("shutdown() body no longer contains `r.historyWg.Wait()`. " +
			"The bounded history-wait is the second half of R44-REL-HIST-GOROUTINE; " +
			"removing it exposes history-loader goroutines to SIGTERM reaping.")
	}

	if cancelIdx[0] >= waitIdx[0] {
		t.Errorf("R172-ARCH-D11 contract broken: `r.historyCancel()` must appear " +
			"BEFORE `r.historyWg.Wait()` inside shutdown(). Without this order, " +
			"in-flight LoadHistory*Ctx calls park on hung filesystem I/O until " +
			"the 5s bounded wait expires, adding 5s to every Shutdown that " +
			"hits that path.")
	}
}
