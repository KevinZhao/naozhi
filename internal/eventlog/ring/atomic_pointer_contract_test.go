package ring

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

// TestLoadAtomicString_NilPointer_ReturnsEmpty matches the session-side
// contract test for loadAtomicString: the untouched zero-value
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
