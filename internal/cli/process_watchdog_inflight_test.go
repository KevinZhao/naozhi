package cli

import "testing"

// TestProcess_clearInflightFlags pins R242-ARCH-27 (#770): the watchdog
// kill-path coordinates with interruptedSettleWindow by clearing both
// settle atomics so a recycled Process struct does not enter
// drainStaleEvents with stale flags that would burn the 500 ms settle
// budget waiting for a result event the killed CLI cannot produce.
func TestProcess_clearInflightFlags(t *testing.T) {
	p := &Process{}
	p.interrupted.Store(true)
	p.interruptedRun.Store(true)

	p.clearInflightFlags()

	if p.interrupted.Load() {
		t.Error("interrupted should be false after clearInflightFlags")
	}
	if p.interruptedRun.Load() {
		t.Error("interruptedRun should be false after clearInflightFlags")
	}

	// Idempotent: a second call on already-cleared flags must not panic
	// (e.g. flipping unrelated state) and must leave both flags false.
	p.clearInflightFlags()
	if p.interrupted.Load() || p.interruptedRun.Load() {
		t.Error("clearInflightFlags must be idempotent")
	}
}
