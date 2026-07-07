package discovery

import (
	"context"
	"testing"
	"time"
)

// TestRefreshDynamicContext_CanceledCtxDoesNotBlockOnSaturatedSem verifies the
// R202606d-GO-002 (#2244) cancellation path: when the prompt-extraction
// semaphore is fully saturated (simulating a slow/hung filesystem keeping all
// in-flight extractions parked) and the caller's context is already canceled,
// RefreshDynamicContext must return promptly instead of parking its fan-out
// goroutines on the semaphore forever. Without the ctx.Done() guard in the
// semaphore-acquire select, wg.Wait() would block indefinitely and this test
// would hit its deadline.
func TestRefreshDynamicContext_CanceledCtxDoesNotBlockOnSaturatedSem(t *testing.T) {
	t.Parallel()
	sc := NewScanner()

	// Saturate the semaphore so every extraction goroutine would otherwise
	// block on `sem <- struct{}{}`.
	for i := 0; i < cap(sc.promptSem); i++ {
		sc.promptSem <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the call

	sessions := []DiscoveredSession{
		{SessionID: "aaaabbbb-0000-0000-0000-000000000001", CWD: "/tmp/x", StartedAt: 1000},
		{SessionID: "aaaabbbb-0000-0000-0000-000000000002", CWD: "/tmp/y", StartedAt: 2000},
	}

	done := make(chan struct{})
	go func() {
		// claudeDir non-empty + non-zero sessions so the body runs the fan-out.
		sc.RefreshDynamicContext(ctx, t.TempDir(), sessions)
		close(done)
	}()

	select {
	case <-done:
		// Returned promptly — cancellation path worked.
	case <-time.After(3 * time.Second):
		t.Fatal("RefreshDynamicContext blocked on saturated semaphore despite canceled ctx (#2244 guard missing)")
	}
}

// TestScanContext_CanceledCtxReturnsPromptly is the Scan-side analogue. There
// are no live candidate processes under a temp claudeDir, but the call must
// still complete (and not hang) with a canceled ctx — exercising the public
// ScanContext wrapper end to end.
func TestScanContext_CanceledCtxReturnsPromptly(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_, _ = sc.ScanContext(ctx, makeClaudeDir(t), nil, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ScanContext did not return promptly with canceled ctx")
	}
}

// TestScan_DelegatesToScanContext is a thin guard that the non-ctx Scan still
// works (delegates with context.Background()) after the #2244 refactor.
func TestScan_DelegatesToScanContext(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	if _, err := sc.Scan(makeClaudeDir(t), nil, nil, nil); err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
}
