package cli

// Process-side half of the Round 170 atomic.Pointer[string] contract. The
// EventLog half, and the loadAtomicString/storeAtomicString tests, went to
// internal/eventlog/ring with the ring itself (#2545 G1) — those helpers were
// never used by Process.

import (
	"reflect"
	"sync/atomic"
	"testing"
)

// TestProcess_DeathReasonIsAtomicPointerString locks the deathReason field
// type. The CAS-based first-writer-wins semantics (setDeathReason) depend on
// atomic.Pointer[string].CompareAndSwap correctly distinguishing nil (never
// stored) from a pointer to "" (explicitly stored empty). Reverting to
// atomic.Value would re-introduce the interface{}-dispatch overhead and allow
// non-string values to slip through at compile time.
func TestProcess_DeathReasonIsAtomicPointerString(t *testing.T) {
	t.Parallel()
	want := reflect.TypeOf(atomic.Pointer[string]{})
	typ := reflect.TypeOf(Process{})
	f, ok := typ.FieldByName("deathReason")
	if !ok {
		t.Fatal("Process.deathReason missing (was it renamed?)")
	}
	if f.Type != want {
		t.Errorf("Process.deathReason type = %v, want atomic.Pointer[string] — "+
			"Round 170 migration removed atomic.Value; setDeathReason CAS depends on nil vs *\"\" distinction",
			f.Type)
	}
}

// TestSetDeathReason_FirstWriterWins exercises the CAS path sequentially.
// Two back-to-back writes: the first CAS succeeds against a nil pointer;
// the second sees a non-nil, non-"" pointer and drops out without writing.
// This verifies the CAS logic itself (first-writer-wins semantics); the
// race-detector covers the genuinely concurrent case at the callers that
// invoke setDeathReason from multiple goroutines.
func TestSetDeathReason_FirstWriterWins(t *testing.T) {
	t.Parallel()
	p := &Process{}
	p.setDeathReason("cli_exited")
	if got := p.DeathReason(); got != "cli_exited" {
		t.Fatalf("after first setDeathReason: got %q, want cli_exited", got)
	}
	// Second writer must not overwrite (CAS fails because ptr is non-nil + non-"").
	p.setDeathReason("readloop_panic")
	if got := p.DeathReason(); got != "cli_exited" {
		t.Errorf("second setDeathReason clobbered first: got %q, want cli_exited — first-writer-wins broken", got)
	}
	// Empty reason is a no-op (guarded at helper entry).
	p.setDeathReason("")
	if got := p.DeathReason(); got != "cli_exited" {
		t.Errorf("empty setDeathReason leaked: got %q, want cli_exited", got)
	}
}

// TestSetDeathReason_UpgradesExplicitEmpty exercises the upgrade-from-empty
// path: if the pointer is non-nil but points to "", a subsequent write must
// install the real reason (no code path stores "" today, but the helper
// tolerates the upgrade for forward compatibility).
func TestSetDeathReason_UpgradesExplicitEmpty(t *testing.T) {
	t.Parallel()
	p := &Process{}
	empty := ""
	p.deathReason.Store(&empty)
	p.setDeathReason("idle_timeout")
	if got := p.DeathReason(); got != "idle_timeout" {
		t.Errorf("upgrade-from-empty: got %q, want idle_timeout", got)
	}
}
