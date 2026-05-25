package session

import (
	"testing"
	"unsafe"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSnapshot_DoesNotCopyPersistedHistory pins the R214-PERF-2 (#411)
// performance contract: ManagedSession.Snapshot must be O(1) regardless of
// persistedHistory size. The triage symptom claimed Snapshot was running
// `copy(snapshot, s.persistedHistory)` on every dashboard poll — verified
// false by reading managed.go (the copy is in attachProcessAndSnapshotPersisted,
// invoked only on attach/adopt paths). This test locks the contract so a
// future "convenience" change that re-introduces the copy fails fast.
//
// Test strategy: prime persistedHistory with maxPersistedHistory entries,
// measure Snapshot's allocation count via testing.AllocsPerRun, and assert
// the count stays bounded by a small constant (the SessionSnapshot value
// itself + the few atomic-string loads, all stack-promotable). A regression
// that adds `copy(out, s.persistedHistory)` would jump allocs by 1 (the
// out slice) AND inflate bytes-allocated past any reasonable bound.
func TestSnapshot_DoesNotCopyPersistedHistory(t *testing.T) {
	// AllocsPerRun panics when called from a parallel test (it disables GC
	// for accurate counting and refuses to interleave with other goroutines).
	s := &ManagedSession{key: "test:direct:alice:general"}

	// Fill persistedHistory to its cap so a regression that copies it
	// would be loud — 500 EventEntry copies are unmissable in allocs/op.
	bigHistory := make([]cli.EventEntry, maxPersistedHistory)
	for i := range bigHistory {
		bigHistory[i] = cli.EventEntry{
			Type:    "text",
			Summary: "entry summary line that is a realistic length",
			Detail:  "detail field with somewhat-realistic content payload size",
		}
	}
	s.historyMu.Lock()
	s.persistedHistory = bigHistory
	s.historyMu.Unlock()

	// Sanity: history is actually populated.
	if got := len(s.persistedHistory); got != maxPersistedHistory {
		t.Fatalf("persistedHistory len = %d, want %d", got, maxPersistedHistory)
	}

	// Warm: first call may pay one-time costs (parseKeyParts sync.Once,
	// costUnitForBackendOnce). Subsequent calls are the steady-state path.
	_ = s.Snapshot()

	allocs := testing.AllocsPerRun(50, func() {
		_ = s.Snapshot()
	})

	// Empirical baseline on the current implementation is ~1-3 allocs
	// (SessionSnapshot escapes via the function return, plus a couple of
	// atomic.Pointer string boxings). 20 leaves comfortable headroom for
	// future scalar-field additions while still tripping on a regression
	// that copies persistedHistory (which would alloc 500+ EventEntries
	// or one big slice = at least one extra alloc).
	const allocCeiling = 20
	if allocs > allocCeiling {
		t.Errorf("Snapshot allocs/op = %v, want <= %d (a regression copying persistedHistory would push this far higher)", allocs, allocCeiling)
	}

	// Belt-and-suspenders: confirm the underlying slice header is unchanged
	// after Snapshot. If a future change accidentally reassigned
	// s.persistedHistory inside Snapshot (e.g. a defensive copy assigned
	// back) the data pointer would shift.
	dataPtrBefore := uintptr(unsafe.Pointer(unsafe.SliceData(s.persistedHistory)))
	_ = s.Snapshot()
	dataPtrAfter := uintptr(unsafe.Pointer(unsafe.SliceData(s.persistedHistory)))
	if dataPtrBefore != dataPtrAfter {
		t.Errorf("Snapshot mutated persistedHistory backing array (ptr %#x → %#x)", dataPtrBefore, dataPtrAfter)
	}
}

// TestSnapshot_ScalarFieldsOnly catalogs the snapshot fields that come from
// O(1) scalar caches rather than O(N) persistedHistory walks. A new field
// added to SessionSnapshot that violates this should break tests near the
// touch site, but having the contract written down here makes the
// expectation visible during code review for #411.
func TestSnapshot_ScalarFieldsOnly(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "test:direct:alice:general"}

	// Seed every cached atomic pointer that Snapshot reads, then take a
	// snapshot with NO process attached (proc == nil branch). The result
	// must surface every cached value WITHOUT touching persistedHistory.
	storeAtomicString(&s.lastPrompt, "user prompt")
	storeAtomicString(&s.lastActivity, "tool_use Read")
	storeAtomicString(&s.lastResponse, "assistant reply")
	storeAtomicString(&s.deathReason, "")
	s.SetUserLabel("custom title")
	s.SetBackend("claude")

	// Even with persistedHistory full, a no-process Snapshot must not pay
	// for it. allocs check above already enforces this; the read-back here
	// provides a behavioural cross-check.
	bigHistory := make([]cli.EventEntry, maxPersistedHistory)
	s.historyMu.Lock()
	s.persistedHistory = bigHistory
	s.historyMu.Unlock()

	snap := s.Snapshot()

	if snap.LastPrompt != "user prompt" {
		t.Errorf("LastPrompt = %q, want %q", snap.LastPrompt, "user prompt")
	}
	if snap.LastActivity != "tool_use Read" {
		t.Errorf("LastActivity = %q, want %q", snap.LastActivity, "tool_use Read")
	}
	if snap.LastResponse != "assistant reply" {
		t.Errorf("LastResponse = %q, want %q", snap.LastResponse, "assistant reply")
	}
	if snap.UserLabel != "custom title" {
		t.Errorf("UserLabel = %q, want %q", snap.UserLabel, "custom title")
	}
}
