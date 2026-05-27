package cli

import (
	"testing"
)

// TestLifecycleContext_NilChannelsCancelsImmediately pins R20260527-GO-13
// (#1289): when both p.done and p.killCh are nil — the legacy &Process{}
// test fixture path — lifecycleContext() must still cancel its allocated
// ctx eagerly so the WithCancel parent slot is freed. Before the fix the
// early-return branch in lifecycleCtxOnce.Do skipped both the watcher
// goroutine spawn AND the cancel() call, leaking the ctx for the test
// binary's lifetime.
func TestLifecycleContext_NilChannelsCancelsImmediately(t *testing.T) {
	t.Parallel()
	p := &Process{} // both done and killCh are nil
	ctx := p.lifecycleContext()
	select {
	case <-ctx.Done():
		// Expected: cancel() ran inline so Done() fires immediately.
	default:
		t.Fatal("lifecycleContext() with nil done/killCh did not cancel its ctx — leak per R20260527-GO-13 (#1289)")
	}
}
