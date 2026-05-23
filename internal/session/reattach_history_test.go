package session

import (
	"strconv"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestReattachProcess_SeedsPersistedHistoryIntoFreshProc pins the bug fix:
// when ReconnectShims attaches a freshly-spawned proc to a session whose
// persistedHistory was already populated by tier1 (naozhilog) before the
// proc existed, the new proc must be seeded with that history. Prior to
// the fix kiro sessions hit this exact path on every naozhi restart and
// the dashboard's default events query returned 0 even though
// persistedHistory carried the conversation.
func TestReattachProcess_SeedsPersistedHistoryIntoFreshProc(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Simulate tier1: persistedHistory is populated while proc is still nil.
	s.InjectHistory([]cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "hello"},
		{Time: 1500, Type: "text", Summary: "hi back"},
	})
	if proc := s.loadProcess(); proc != nil {
		t.Fatalf("preconditions: proc should be nil before Reattach, got %T", proc)
	}

	// Now the reconnect path attaches a fresh, empty proc.
	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "session-uuid")

	// The fresh proc.EventLog must now carry the historical entries so
	// EventEntries() (which prefers proc when non-nil) returns them.
	got := s.EventEntries()
	if len(got) != 2 {
		t.Fatalf("EventEntries() len=%d want 2; new proc was not seeded", len(got))
	}
	if got[0].Summary != "hello" || got[1].Summary != "hi back" {
		t.Errorf("EventEntries() content mismatch: %+v", got)
	}
}

// TestInjectHistory_AfterReattach_DoesNotDoubleInject checks the symmetric
// race: tier1 fires AFTER ReconnectShims attached a fresh proc. The catch-up
// inside ReattachProcess must not collide with InjectHistory's own
// "forward to proc" step and result in 2× the entries appearing in the ring.
func TestInjectHistory_AfterReattach_DoesNotDoubleInject(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}

	// Reattach BEFORE any history is injected — empty snapshot.
	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "session-uuid")

	// Now tier1 lands its batch.
	s.InjectHistory([]cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "msg-a"},
		{Time: 2000, Type: "user", Summary: "msg-b"},
	})

	got := s.EventEntries()
	if len(got) != 2 {
		t.Fatalf("EventEntries() len=%d want 2 (entries inserted once)", len(got))
	}
}

// TestReattachProcess_AfterInjectHistory_DoesNotDoubleInject covers the
// reverse race: tier1 wins the lock first, then ReattachProcess runs.
// persistedSeededLen must be set so the catch-up snapshot covers the
// already-injected entries exactly once, while the InjectHistory caller
// (which observed proc=nil under the lock) didn't forward anything.
func TestReattachProcess_AfterInjectHistory_DoesNotDoubleInject(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "msg-a"},
		{Time: 2000, Type: "user", Summary: "msg-b"},
	})
	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "uuid")

	got := s.EventEntries()
	if len(got) != 2 {
		t.Fatalf("EventEntries() len=%d want 2 (entries inserted once)", len(got))
	}
}

// TestInjectHistory_NewTailAfterReattach_ForwardsOnlyTail covers the steady-
// state path: after Reattach already seeded historical entries, a fresh
// InjectHistory batch (e.g. cron tail / system event) must forward only the
// new tail, not duplicate the seeded prefix.
func TestInjectHistory_NewTailAfterReattach_ForwardsOnlyTail(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "old-1"},
		{Time: 2000, Type: "user", Summary: "old-2"},
	})
	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "uuid")

	// Now a new tail arrives.
	s.InjectHistory([]cli.EventEntry{
		{Time: 3000, Type: "user", Summary: "new-3"},
	})

	got := s.EventEntries()
	if len(got) != 3 {
		t.Fatalf("EventEntries() len=%d want 3 (2 old + 1 new), got %+v", len(got), got)
	}
	if got[2].Summary != "new-3" {
		t.Errorf("tail entry mismatch: %+v", got[2])
	}
}

// TestReattachProcess_ConcurrentInjectHistory exercises the historyMu
// serialisation under -race. Spinning many concurrent InjectHistory calls
// against a single Reattach must end with exactly the union of entries in
// the ring — no duplicates, no losses.
func TestReattachProcess_ConcurrentInjectHistory(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	const writers = 8
	const perWriter = 25

	var wg sync.WaitGroup
	wg.Add(writers + 1)

	go func() {
		defer wg.Done()
		// Reattach somewhere in the middle of the writer storm.
		proc := NewTestProcess()
		s.ReattachProcessNoCallback(proc, "uuid")
	}()

	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				s.InjectHistory([]cli.EventEntry{{
					Time: int64(w*1000 + i),
					Type: "user",
					// Distinct summaries: writer index and sequence
					// so the union check below is exact.
					Summary: "w" + strconv.Itoa(w) + "i" + strconv.Itoa(i),
				}})
			}
		}()
	}
	wg.Wait()

	got := s.EventEntries()
	want := writers * perWriter
	if len(got) != want {
		t.Fatalf("EventEntries() len=%d want %d (no double-inject, no drop)", len(got), want)
	}
	seen := make(map[string]int, want)
	for _, e := range got {
		seen[e.Summary]++
	}
	for sum, n := range seen {
		if n != 1 {
			t.Errorf("summary %q appeared %d times; want exactly 1", sum, n)
		}
	}
}

// TestEventEntries_DefaultEndpoint_ReturnsFullHistoryAfterReattach is the
// end-to-end pin for the SmartRenew kiro bug:
//
//   - tier1 (naozhilog) populates persistedHistory while proc is still nil
//   - ReconnectShims attaches a fresh proc (its EventLog is empty)
//   - Dashboard calls /api/sessions/events with no limit/before/after,
//     which routes through s.EventEntries() — the live-proc branch
//
// Pre-fix: the empty new proc.EventLog gives 0 entries, history vanishes
// from the dashboard until the operator triggers a disk-fallback path.
// Post-fix: ReattachProcess seeds proc.EventLog from persistedHistory, so
// EventEntries() returns the full conversation.
func TestEventEntries_DefaultEndpoint_ReturnsFullHistoryAfterReattach(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	historic := []cli.EventEntry{
		{Time: 100, Type: "user", Summary: "first user"},
		{Time: 200, Type: "text", Summary: "first reply"},
		{Time: 300, Type: "user", Summary: "second user"},
	}
	// tier1 lands history while proc is nil.
	s.InjectHistory(historic)

	// ReconnectShims-style reattach with a brand-new (empty) proc.
	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "session-uuid")

	// Default-endpoint path: handleGet routes (no after, no before, no limit)
	// into sess.EventEntries(); pre-fix this returned 0.
	got := s.EventEntries()
	if len(got) != len(historic) {
		t.Fatalf("EventEntries() len=%d want %d (default path lost kiro history)",
			len(got), len(historic))
	}
	for i := range got {
		if got[i].Summary != historic[i].Summary {
			t.Errorf("entry[%d].Summary=%q want %q", i, got[i].Summary, historic[i].Summary)
		}
	}

	// And after one more turn lands on the live proc, EventEntries() must
	// return historic + new — not just the new entry, which is what the
	// post-fix mem-tier branch in EventEntriesBeforeCtx also relies on.
	proc.EventLog.Append(cli.EventEntry{Time: 400, Type: "user", Summary: "post-restart turn"})
	got = s.EventEntries()
	if len(got) != len(historic)+1 {
		t.Fatalf("after live turn len=%d want %d", len(got), len(historic)+1)
	}
	if got[len(got)-1].Summary != "post-restart turn" {
		t.Errorf("last entry=%q want post-restart turn", got[len(got)-1].Summary)
	}
}

// TestReattachProcess_PersistedHistoryCapTrim covers R231-CQ-8: previous
// reattach tests hand in 200 entries, well below maxPersistedHistory=500,
// so the cap-trim branch in InjectHistory was never exercised. Push past
// the cap and assert (a) persistedHistory does not grow past the limit,
// (b) FIFO eviction keeps the most-recent tail entries, and (c) a
// follow-up tail injection neither duplicates the seeded prefix nor
// overshoots the cap.
func TestReattachProcess_PersistedHistoryCapTrim(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}

	const total = maxPersistedHistory + 250
	batch := make([]cli.EventEntry, total)
	for i := range batch {
		batch[i] = cli.EventEntry{
			Time:    int64(i + 1),
			Type:    "user",
			Summary: "seq-" + strconv.Itoa(i),
		}
	}
	s.InjectHistory(batch)

	s.historyMu.RLock()
	persisted := len(s.persistedHistory)
	s.historyMu.RUnlock()
	if persisted != maxPersistedHistory {
		t.Fatalf("persistedHistory=%d want cap=%d after over-fill", persisted, maxPersistedHistory)
	}

	proc := NewTestProcess()
	s.ReattachProcessNoCallback(proc, "uuid-cap-trim")

	got := s.EventEntries()
	if len(got) != maxPersistedHistory {
		t.Fatalf("EventEntries() len=%d want %d (cap-trimmed window)", len(got), maxPersistedHistory)
	}
	wantFirst := "seq-" + strconv.Itoa(total-maxPersistedHistory)
	wantLast := "seq-" + strconv.Itoa(total-1)
	if got[0].Summary != wantFirst {
		t.Errorf("first entry=%q want %q (oldest after FIFO trim)", got[0].Summary, wantFirst)
	}
	if got[len(got)-1].Summary != wantLast {
		t.Errorf("last entry=%q want %q", got[len(got)-1].Summary, wantLast)
	}

	tail := []cli.EventEntry{
		{Time: int64(total + 1), Type: "user", Summary: "post-cap-1"},
		{Time: int64(total + 2), Type: "user", Summary: "post-cap-2"},
	}
	s.InjectHistory(tail)

	got = s.EventEntries()
	if len(got) > maxPersistedHistory {
		t.Fatalf("after second inject len=%d exceeds cap=%d", len(got), maxPersistedHistory)
	}
	if got[len(got)-1].Summary != "post-cap-2" {
		t.Errorf("last entry after second inject=%q want post-cap-2", got[len(got)-1].Summary)
	}
	seen := make(map[string]int, len(got))
	for _, e := range got {
		seen[e.Summary]++
	}
	for sum, n := range seen {
		if n != 1 {
			t.Errorf("summary %q appeared %d times; cap-trim must not duplicate", sum, n)
		}
	}
}
