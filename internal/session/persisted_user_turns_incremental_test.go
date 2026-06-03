package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// countUserTurns is the O(n) oracle: the exact value the removed
// recountPersistedUserTurnsLocked full scan would produce over the current
// persistedHistory slice. The incremental count maintained by InjectHistory
// (R20260603140013-PERF-2) must always agree with this.
func countUserTurns(hist []cli.EventEntry) int64 {
	var n int64
	for i := range hist {
		if hist[i].Type == "user" {
			n++
		}
	}
	return n
}

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

// TestInjectHistory_IncrementalCount_PureAppend covers repeated sub-cap appends
// with a mix of user / non-user entries: the incremental count must equal the
// full rescan after every InjectHistory.
func TestInjectHistory_IncrementalCount_PureAppend(t *testing.T) {
	s := &ManagedSession{key: "k"}
	batches := [][]cli.EventEntry{
		{{Time: 1, Type: "user"}, {Time: 2, Type: "text"}},
		{{Time: 3, Type: "user"}, {Time: 4, Type: "user"}, {Time: 5, Type: "init"}},
		{{Time: 6, Type: "tool_use"}},
		{{Time: 7, Type: "user"}},
	}
	for _, b := range batches {
		s.InjectHistory(b)
		assertCountMatchesOracle(t, s)
	}
	if got := s.persistedUserTurns.Load(); got != 4 {
		t.Fatalf("final count = %d, want 4", got)
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
		s.InjectHistory([]cli.EventEntry{{Time: ts, Type: typ}})
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

// TestInjectHistory_IncrementalCount_OverCapSingleBatch covers a single batch
// larger than maxPersistedHistory (boot-time JSONL replay of a huge file):
// InjectHistory truncates the batch to the last maxPersistedHistory entries
// (line ~306) before appending, so the cached count must reflect only the
// retained tail's user entries.
func TestInjectHistory_IncrementalCount_OverCapSingleBatch(t *testing.T) {
	s := &ManagedSession{key: "k"}
	total := maxPersistedHistory + 137
	entries := make([]cli.EventEntry, total)
	for i := range entries {
		typ := "text"
		if i%3 == 0 {
			typ = "user"
		}
		entries[i] = cli.EventEntry{Time: int64(i + 1), Type: typ}
	}
	s.InjectHistory(entries)
	assertCountMatchesOracle(t, s)
	// Sanity: the retained window is exactly the last maxPersistedHistory items.
	wantTail := countUserTurns(entries[total-maxPersistedHistory:])
	if got := s.persistedUserTurns.Load(); got != wantTail {
		t.Fatalf("count = %d, want %d (users in retained tail)", got, wantTail)
	}
}

// TestInjectHistory_IncrementalCount_SecondBatchTriggersTrim covers the mixed
// case where an existing sub-cap history is pushed over the cap by a second
// large batch, so the trimmed prefix spans the boundary between old and new
// entries — the trickiest equivalence case for the increment/decrement math.
func TestInjectHistory_IncrementalCount_SecondBatchTriggersTrim(t *testing.T) {
	s := &ManagedSession{key: "k"}
	first := make([]cli.EventEntry, 300)
	for i := range first {
		typ := "text"
		if i%2 == 0 {
			typ = "user"
		}
		first[i] = cli.EventEntry{Time: int64(i + 1), Type: typ}
	}
	s.InjectHistory(first)
	assertCountMatchesOracle(t, s)

	second := make([]cli.EventEntry, 400)
	for i := range second {
		typ := "tool_use"
		if i%4 == 0 {
			typ = "user"
		}
		second[i] = cli.EventEntry{Time: int64(1000 + i), Type: typ}
	}
	s.InjectHistory(second)
	assertCountMatchesOracle(t, s)
}

// TestInjectHistory_IncrementalCount_EmptyBatch ensures a no-op empty batch
// leaves the count untouched and still oracle-consistent.
func TestInjectHistory_IncrementalCount_EmptyBatch(t *testing.T) {
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]cli.EventEntry{{Time: 1, Type: "user"}})
	s.InjectHistory(nil)
	s.InjectHistory([]cli.EventEntry{})
	assertCountMatchesOracle(t, s)
	if got := s.persistedUserTurns.Load(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}
