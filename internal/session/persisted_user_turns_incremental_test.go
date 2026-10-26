package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// The O(n) oracle is the production countUserTurns helper (respawn_snapshot.go):
// the exact value a full scan of persistedHistory produces. The incremental
// count maintained by InjectHistory (R20260603140013-PERF-2) must always agree
// with it.

// assertCountMatchesOracle checks that the cached persistedUserTurns equals a
// fresh full scan of persistedHistory (the recount oracle). Holds historyMu to
// read the slice consistently, mirroring the production read contract.
func assertCountMatchesOracle(t *testing.T, s *ManagedSession) {
	t.Helper()
	s.historyMu.RLock()
	want := countUserTurns(s.persistedHistory)
	s.historyMu.RUnlock()
	if got := s.persistedUserTurns.Load(); got != want {
		t.Fatalf("persistedUserTurns = %d, full-recount oracle = %d", got, want)
	}
}

// TestInjectHistory_IncrementalCount_AppendThenTrim drives persistedHistory past
// maxPersistedHistory via many small appends so the cap-trim prefix-drop path
// fires repeatedly. The dropped prefix carries user entries, so the decrement
// branch must keep the cached count equal to the oracle.
func TestInjectHistory_IncrementalCount_AppendThenTrim(t *testing.T) {
	s := &ManagedSession{key: "k"}
	var ts int64
	// Alternate user / text so trimmed prefixes contain a known share of users.
	for i := 0; i < maxPersistedHistory+200; i++ {
		ts++
		typ := "text"
		if i%2 == 0 {
			typ = "user"
		}
		s.InjectHistory([]clievent.EventEntry{{Time: ts, Type: typ}})
		assertCountMatchesOracle(t, s)
	}
	// After trimming, persistedHistory holds exactly maxPersistedHistory entries.
	s.historyMu.RLock()
	if n := len(s.persistedHistory); n != maxPersistedHistory {
		s.historyMu.RUnlock()
		t.Fatalf("len(persistedHistory) = %d, want %d", n, maxPersistedHistory)
	}
	s.historyMu.RUnlock()
}
