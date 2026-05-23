package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestSetPrevSessionOrigins_FreshAttach pins the simplest happy path:
// a brand-new session has no prior origins; SetPrevSessionOrigins for
// a freshly-attached chain must produce a fully populated parallel
// slice, all entries stamped with the supplied label.
func TestSetPrevSessionOrigins_FreshAttach(t *testing.T) {
	s := &ManagedSession{key: "test:d:u:general"}
	ids := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
	}
	s.historyMu.Lock()
	s.prevSessionIDs = append(s.prevSessionIDs, ids...)
	s.historyMu.Unlock()

	s.SetPrevSessionOrigins(ids, "auto-spawn")

	got := s.SnapshotPrevSessionOrigins()
	if len(got) != 2 {
		t.Fatalf("len(origins) = %d, want 2", len(got))
	}
	for i, o := range got {
		if o != "auto-spawn" {
			t.Errorf("origins[%d] = %q, want auto-spawn", i, o)
		}
	}
}

// TestSetPrevSessionOrigins_PreservesPriorPrefix exercises the chain-
// rotation case: an existing chain (with a prior origin label or
// implicit "manual") gains additional entries with a different origin.
// The trailing block must adopt the new label; the prefix must keep
// its prior label (or default to "manual" when none was recorded).
func TestSetPrevSessionOrigins_PreservesPriorPrefix(t *testing.T) {
	s := &ManagedSession{key: "test:d:u:general"}
	priorIDs := []string{"00000000-0000-4000-8000-0000000000aa"}
	newIDs := []string{
		"00000000-0000-4000-8000-0000000000bb",
		"00000000-0000-4000-8000-0000000000cc",
	}
	s.historyMu.Lock()
	s.prevSessionIDs = append(s.prevSessionIDs, priorIDs...)
	// Simulate a manual prior assignment (legacy or /clear path).
	s.prevSessionOrigins = []string{"manual"}
	s.prevSessionIDs = append(s.prevSessionIDs, newIDs...)
	s.historyMu.Unlock()

	s.SetPrevSessionOrigins(newIDs, "auto-backfill")

	got := s.SnapshotPrevSessionOrigins()
	if len(got) != 3 {
		t.Fatalf("len(origins) = %d, want 3", len(got))
	}
	if got[0] != "manual" {
		t.Errorf("origins[0] = %q, want manual (preserved)", got[0])
	}
	if got[1] != "auto-backfill" || got[2] != "auto-backfill" {
		t.Errorf("origins[1..2] = %v, want both auto-backfill", got[1:])
	}
}

// TestSetPrevSessionOrigins_LengthDriftRecovery is the v3 Arch-MINOR-1
// invariant test: if the parallel slices fall out of sync (origins
// longer than ids), SetPrevSessionOrigins MUST rebuild origins to
// all-"manual" rather than persisting misaligned labels. The metric
// counter must tick so a future regression is observable.
func TestSetPrevSessionOrigins_LengthDriftRecovery(t *testing.T) {
	before := metrics.AutoChainOriginsLengthMismatch.Value()

	s := &ManagedSession{key: "test:d:u:general"}
	s.historyMu.Lock()
	s.prevSessionIDs = []string{"00000000-0000-4000-8000-000000000001"}
	// Drift: origins is longer than ids — would mean a past write
	// removed an id without removing its origin.
	s.prevSessionOrigins = []string{"manual", "auto-spawn", "auto-backfill"}
	s.historyMu.Unlock()

	// Caller appends one new id and tries to label it.
	newIDs := []string{"00000000-0000-4000-8000-000000000002"}
	s.historyMu.Lock()
	s.prevSessionIDs = append(s.prevSessionIDs, newIDs...)
	s.historyMu.Unlock()

	s.SetPrevSessionOrigins(newIDs, "auto-spawn")

	if got := metrics.AutoChainOriginsLengthMismatch.Value(); got != before+1 {
		t.Errorf("AutoChainOriginsLengthMismatch = %d, want %d", got, before+1)
	}
	got := s.SnapshotPrevSessionOrigins()
	if len(got) != 2 {
		t.Fatalf("len(origins) = %d, want 2", len(got))
	}
	if got[0] != "manual" {
		t.Errorf("origins[0] = %q, want manual (rebuilt)", got[0])
	}
	if got[1] != "auto-spawn" {
		t.Errorf("origins[1] = %q, want auto-spawn (newly stamped)", got[1])
	}
}

// TestSnapshotPrevSessionOrigins_DefaultsToManual: when origins is
// shorter than ids (legacy session restored from disk), Snapshot
// returns a defensive copy with "manual" filling any unset positions.
func TestSnapshotPrevSessionOrigins_DefaultsToManual(t *testing.T) {
	s := &ManagedSession{key: "test:d:u:general"}
	s.historyMu.Lock()
	s.prevSessionIDs = []string{"a", "b", "c"}
	// origins absent — common case for sessions restored from a
	// pre-feature sessions.json.
	s.historyMu.Unlock()

	got := s.SnapshotPrevSessionOrigins()
	if len(got) != 3 {
		t.Fatalf("len(origins) = %d, want 3", len(got))
	}
	for i, o := range got {
		if o != "manual" {
			t.Errorf("origins[%d] = %q, want manual", i, o)
		}
	}
}

// TestSetPrevSessionOrigins_EmptyOriginNoop: caller bug protection.
// Empty origin string must not change anything (and must not panic).
func TestSetPrevSessionOrigins_EmptyOriginNoop(t *testing.T) {
	s := &ManagedSession{key: "test:d:u:general"}
	s.historyMu.Lock()
	s.prevSessionIDs = []string{"a"}
	s.prevSessionOrigins = []string{"manual"}
	s.historyMu.Unlock()

	s.SetPrevSessionOrigins([]string{"a"}, "")
	got := s.SnapshotPrevSessionOrigins()
	if len(got) != 1 || got[0] != "manual" {
		t.Errorf("expected origins unchanged on empty origin; got %v", got)
	}
}

// TestSetPrevSessionOrigins_EmptyIDsNoop matches the empty-origin guard:
// passing nil/empty ids must short-circuit cleanly.
func TestSetPrevSessionOrigins_EmptyIDsNoop(t *testing.T) {
	s := &ManagedSession{key: "test:d:u:general"}
	s.historyMu.Lock()
	s.prevSessionIDs = []string{"a"}
	s.prevSessionOrigins = []string{"manual"}
	s.historyMu.Unlock()

	s.SetPrevSessionOrigins(nil, "auto-spawn")
	got := s.SnapshotPrevSessionOrigins()
	if len(got) != 1 || got[0] != "manual" {
		t.Errorf("expected origins unchanged on empty ids; got %v", got)
	}
}

// TestSnapshotPrevSessionOrigins_EmptyChain returns nil (not zero-length
// non-nil), matching the contract used by SnapshotChainIDs.
func TestSnapshotPrevSessionOrigins_EmptyChain(t *testing.T) {
	s := &ManagedSession{key: "test:d:u:general"}
	if got := s.SnapshotPrevSessionOrigins(); got != nil {
		t.Fatalf("expected nil for empty chain, got %v", got)
	}
}
