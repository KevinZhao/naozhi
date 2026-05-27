package cli

import (
	"testing"
	"time"
)

// TestLifecycleContext_NilDoneAndKillCh_CancelsImmediately pins
// R20260527-GO-13 (#1289): when both p.done and p.killCh are nil (legacy
// test fixture path), lifecycleContext historically returned a live
// context.WithCancel whose cancel was never called — leaking the parent
// goroutine's cancelCtx child slot for the test's lifetime.
//
// The fix cancels synchronously in the early-return branch so callers
// that observe ctx.Done() see an immediately-closed channel, matching
// the "no lifetime signal will ever fire" semantics.
func TestLifecycleContext_NilDoneAndKillCh_CancelsImmediately(t *testing.T) {
	t.Parallel()

	p := &Process{}
	ctx := p.lifecycleContext()
	if ctx == nil {
		t.Fatal("lifecycleContext returned nil ctx")
	}

	select {
	case <-ctx.Done():
		// expected — early-return branch cancelled synchronously
	case <-time.After(100 * time.Millisecond):
		t.Fatal("lifecycleContext leaked: ctx.Done() did not fire when both done and killCh are nil")
	}
}
