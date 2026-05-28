package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSnapshotPrevSessionIDs_DefensiveClone asserts the accessor returns a
// fresh copy that callers can mutate without disturbing the session's
// internal state. R215-ARCH-P1-1 (#545) — pre-requisite for splitting
// Router into sub-aggregates.
func TestSnapshotPrevSessionIDs_DefensiveClone(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{prevSessionIDs: []string{"id-a", "id-b"}}

	got := s.SnapshotPrevSessionIDs()
	if len(got) != 2 || got[0] != "id-a" || got[1] != "id-b" {
		t.Fatalf("SnapshotPrevSessionIDs = %v, want [id-a id-b]", got)
	}
	got[0] = "MUTATED"
	if s.prevSessionIDs[0] != "id-a" {
		t.Errorf("source mutated through returned slice: %v", s.prevSessionIDs)
	}
}

// TestSnapshotPrevSessionIDs_EmptyReturnsNil keeps the no-zero-len-alloc
// contract that matches SnapshotPrevSessionOrigins.
func TestSnapshotPrevSessionIDs_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{}
	if got := s.SnapshotPrevSessionIDs(); got != nil {
		t.Errorf("SnapshotPrevSessionIDs on empty = %v, want nil", got)
	}
}

// TestReplacePrevSessionIDs_ClonesInput asserts the setter clones its
// argument so subsequent caller mutation cannot reach into the session.
func TestReplacePrevSessionIDs_ClonesInput(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{}
	src := []string{"x", "y", "z"}
	s.ReplacePrevSessionIDs(src)
	src[0] = "MUTATED"
	got := s.SnapshotPrevSessionIDs()
	if len(got) != 3 || got[0] != "x" {
		t.Errorf("ReplacePrevSessionIDs did not clone input; got %v", got)
	}
}

// TestReplacePrevSessionIDs_EmptyClears verifies len-0 input clears the
// chain rather than retaining the previous content.
func TestReplacePrevSessionIDs_EmptyClears(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{prevSessionIDs: []string{"old"}}
	s.ReplacePrevSessionIDs(nil)
	if got := s.SnapshotPrevSessionIDs(); got != nil {
		t.Errorf("ReplacePrevSessionIDs(nil) leaked old chain: %v", got)
	}
}

// TestSnapshotPersistedHistory_DefensiveCopy asserts the result is a fresh
// slice safely mutated without touching the session's ring.
func TestSnapshotPersistedHistory_DefensiveCopy(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{
		persistedHistory: []cli.EventEntry{
			{Type: "user", Summary: "first"},
			{Type: "assistant", Summary: "second"},
		},
	}
	got := s.SnapshotPersistedHistory()
	if len(got) != 2 || got[0].Summary != "first" {
		t.Fatalf("SnapshotPersistedHistory = %v, want 2 entries", got)
	}
	got[0].Summary = "MUTATED"
	if s.persistedHistory[0].Summary != "first" {
		t.Errorf("source mutated through returned slice: %v", s.persistedHistory)
	}
}

// TestSnapshotPersistedHistory_EmptyReturnsNil keeps the no-zero-len-alloc
// contract.
func TestSnapshotPersistedHistory_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{}
	if got := s.SnapshotPersistedHistory(); got != nil {
		t.Errorf("SnapshotPersistedHistory on empty = %v, want nil", got)
	}
}
