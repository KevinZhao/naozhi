package cron

import (
	"testing"
)

// TestAbortChanPool_DrainsBeforeRecycle pins the R20260607-PERF-001 (#1921)
// reuse-safety invariant: putAbortChan must non-blockingly drain any residual
// value so a recycled channel never bleeds a stale abortResult into the next
// getAbortChan user. A caller that forgets to drain (or a future refactor that
// reorders the drain) would otherwise hand the next tick a phantom fired=true.
func TestAbortChanPool_DrainsBeforeRecycle(t *testing.T) {
	ch := getAbortChan()
	// Simulate a callback that published but was never drained by the caller.
	ch <- abortResult{outcome: InterruptSent, fired: true}
	putAbortChan(ch)

	// The very next Get must observe an empty channel — not the stale value.
	got := getAbortChan()
	select {
	case v := <-got:
		t.Fatalf("recycled channel leaked a stale value %+v; putAbortChan must drain", v)
	default:
	}
	// And it must still be a usable buffer=1 channel.
	if cap(got) != 1 {
		t.Fatalf("recycled channel cap = %d, want 1", cap(got))
	}
	got <- abortResult{}
	select {
	case <-got:
	default:
		t.Fatal("recycled channel did not accept + deliver a fresh send")
	}
}

// TestAbortChanPool_EmptyChannelRecycleIsSafe verifies the common
// stop()==true path — where the channel was never written — recycles
// without blocking on the empty non-blocking drain.
func TestAbortChanPool_EmptyChannelRecycleIsSafe(t *testing.T) {
	ch := getAbortChan()
	// No send: mirrors sendWithWatchdog's stop()==true branch.
	putAbortChan(ch)
	got := getAbortChan()
	if cap(got) != 1 {
		t.Fatalf("recycled channel cap = %d, want 1", cap(got))
	}
	select {
	case v := <-got:
		t.Fatalf("clean recycled channel had a value %+v; want empty", v)
	default:
	}
}
