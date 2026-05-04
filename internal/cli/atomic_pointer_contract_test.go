package cli

import (
	"reflect"
	"sync/atomic"
	"testing"
)

// TestEventLog_AtomicPointerStringFields pins the Round 170 migration of
// lastPromptSummary / lastActivitySummary from atomic.Value to
// atomic.Pointer[string]. Runtime reflect check catches a field-type revert;
// the sibling test in session/atomic_pointer_contract_test.go handles the
// cross-package grep for legacy atomic.Value field declarations.
func TestEventLog_AtomicPointerStringFields(t *testing.T) {
	t.Parallel()
	want := reflect.TypeOf(atomic.Pointer[string]{})
	typ := reflect.TypeOf(EventLog{})
	for _, name := range []string{"lastPromptSummary", "lastActivitySummary"} {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("EventLog.%s missing (was it renamed?)", name)
			continue
		}
		if f.Type != want {
			t.Errorf("EventLog.%s type = %v, want atomic.Pointer[string] — "+
				"Round 170 migration removed atomic.Value; do not revert",
				name, f.Type)
		}
	}
}

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

// TestLoadAtomicString_NilPointer_ReturnsEmpty matches the session-side
// contract test for loadStringAtomic: the untouched zero-value
// atomic.Pointer[string] must collapse to "" on read.
func TestLoadAtomicString_NilPointer_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if got := loadAtomicString(&v); got != "" {
		t.Errorf("loadAtomicString(nil ptr) = %q, want \"\"", got)
	}
}

// TestStoreAtomicString_SkipsEqualWrite pins the R176-PERF-P1 compare-before-store
// fast path on the cli-package helper. EventLog.Append invokes this helper
// for every user / tool_use / thinking / agent / task_start / task_progress
// / todo event under l.mu; high-frequency cron + long-session workloads
// repeatedly store the same Summary string (the same Bash one-liner across
// a 50-step turn). The fast path avoids per-call *string allocation and
// spurious atomic writes on a cache line that the Snapshot / LastPromptSummary
// / LastActivitySummary readers poll at high rates.
func TestStoreAtomicString_SkipsEqualWrite(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	storeAtomicString(&v, "Bash")
	firstPtr := v.Load()
	if firstPtr == nil || *firstPtr != "Bash" {
		t.Fatalf("first store failed: got %v", firstPtr)
	}
	storeAtomicString(&v, "Bash")
	if got := v.Load(); got != firstPtr {
		t.Errorf("equal-value second store allocated a new pointer (%p != %p) — fast path regression", got, firstPtr)
	}
	storeAtomicString(&v, "Read")
	if got := v.Load(); got == firstPtr {
		t.Errorf("divergent store skipped write — compare-before-store semantics broken")
	}
	if got := loadAtomicString(&v); got != "Read" {
		t.Errorf("after divergent store: got %q, want \"Read\"", got)
	}
}

// TestStoreAtomicString_NilToEmptyIsNotSkipped: first write of "" from a
// never-stored pointer must install a non-nil pointer so downstream code
// can distinguish "explicit empty" from "never written".
func TestStoreAtomicString_NilToEmptyIsNotSkipped(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if v.Load() != nil {
		t.Fatal("precondition: zero-value atomic.Pointer must Load() nil")
	}
	storeAtomicString(&v, "")
	if v.Load() == nil {
		t.Error("first store of \"\" from nil pointer was skipped — fast path must not short-circuit when cur==nil")
	}
}
