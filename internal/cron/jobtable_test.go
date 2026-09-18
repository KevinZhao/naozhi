package cron

import (
	"sync"
	"testing"
)

// newTestTable builds a table with jobs registered through the same seams
// production uses, so the derived indices are maintained the way they are at
// runtime rather than by the test.
func newTestTable(t *testing.T, jobs ...*Job) *Scheduler {
	t.Helper()
	s := &Scheduler{jobTable: newJobTable()}
	for _, j := range jobs {
		s.jobs[j.ID] = j
		s.addToChatIndexLocked(j)
	}
	return s
}

func job(id, platform, chat string) *Job {
	return &Job{ID: id, Platform: platform, ChatID: chat}
}

// TestJobTable_ReadsAgreeWithTheMap is the invariant the four fields exist to
// uphold: chatJobCount is the per-chat cardinality of jobs, and sortedJobIDs is
// its key order. A read API answering from the derived index while the map says
// otherwise is the failure this catches — and it is the shape of the mutation
// "a writer updates jobs but forgets an index".
func TestJobTable_ReadsAgreeWithTheMap(t *testing.T) {
	s := newTestTable(t,
		job("aaa", "wecom", "chat-1"),
		job("bbb", "wecom", "chat-1"),
		job("ccc", "wecom", "chat-2"),
	)

	if got := s.count(); got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
	if got := s.countForChat(chatKeyFor("wecom", "chat-1")); got != 2 {
		t.Errorf("countForChat(chat-1) = %d, want 2 — the cap check would let a third job in", got)
	}
	if got := s.countForChat(chatKeyFor("wecom", "chat-2")); got != 1 {
		t.Errorf("countForChat(chat-2) = %d, want 1", got)
	}
	if got := s.countForChat(chatKeyFor("wecom", "chat-none")); got != 0 {
		t.Errorf("countForChat(unknown) = %d, want 0", got)
	}

	// ids must be the map's keys, in order, and must not alias the table.
	ids := s.ids()
	want := []string{"aaa", "bbb", "ccc"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	ids[0] = "mutated-by-caller"
	if s.ids()[0] != "aaa" {
		t.Error("ids() aliases sortedJobIDs: a caller mutating the result corrupted the table")
	}
}

// TestJobTable_ExistsAndLiveness covers the two answers callers actually ask for,
// including the pairing: liveness reports registration and pause together because
// a caller that asked twice could see a job registered and then, a moment later,
// not paused — a run that should have been skipped.
func TestJobTable_ExistsAndLiveness(t *testing.T) {
	paused := job("bbb", "wecom", "c")
	paused.Paused = true
	s := newTestTable(t, job("aaa", "wecom", "c"), paused)

	if !s.exists("aaa") || !s.exists("bbb") {
		t.Error("exists false for a registered job")
	}
	if s.exists("zzz") {
		t.Error("exists true for an unregistered job")
	}

	if reg, p := s.liveness("aaa"); !reg || p {
		t.Errorf("liveness(aaa) = (%v, %v), want (true, false)", reg, p)
	}
	if reg, p := s.liveness("bbb"); !reg || !p {
		t.Errorf("liveness(bbb) = (%v, %v), want (true, true)", reg, p)
	}
	// Unregistered must not report paused: a caller branching on paused first
	// would log "paused, skipping" for a job that was deleted.
	if reg, p := s.liveness("zzz"); reg || p {
		t.Errorf("liveness(zzz) = (%v, %v), want (false, false)", reg, p)
	}
}

func TestJobTable_LastSessionID(t *testing.T) {
	j := job("aaa", "wecom", "c")
	j.LastSessionID = "sess-1"
	s := newTestTable(t, j)

	if got, ok := s.lastSessionID("aaa"); !ok || got != "sess-1" {
		t.Errorf("lastSessionID = (%q, %v), want (sess-1, true)", got, ok)
	}
	if _, ok := s.lastSessionID("zzz"); ok {
		t.Error("lastSessionID reported ok for an unregistered job")
	}
}

// TestJobTable_SaveSeqIsMonotonicUnderConcurrency: saveSeq's only job is to
// total-order snapshots, so two concurrent assignments must never collide. This
// is why the counter moved into the table with the lock rather than staying on
// Scheduler — a seq assigned under a lock that no longer serialises it against
// the snapshot it tags would order nothing.
func TestJobTable_SaveSeqIsMonotonicUnderConcurrency(t *testing.T) {
	s := &Scheduler{jobTable: newJobTable()}
	const n = 200
	seen := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = s.nextSaveSeq()
		}(i)
	}
	wg.Wait()

	uniq := make(map[uint64]bool, n)
	for _, v := range seen {
		if v == 0 {
			t.Fatal("nextSaveSeq returned 0; zero is the never-saved sentinel lastSavedSeq compares against")
		}
		if uniq[v] {
			t.Fatalf("seq %d assigned twice; a stale snapshot could overwrite a newer one on disk", v)
		}
		uniq[v] = true
	}
}
