package cron

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// tableWith returns a table holding jobs, indexed as the add path indexes them.
func tableWith(t *testing.T, jobs ...*Job) *jobTable {
	t.Helper()
	tbl := newJobTable(nil)
	tbl.mu.Lock()
	for _, j := range jobs {
		tbl.jobs[j.ID] = j
		tbl.indexLocked(j)
	}
	tbl.mu.Unlock()
	return &tbl
}

// The read API hands out copies: changing what it returned changes nothing in
// the table.
func TestJobTableReads_AreCopies(t *testing.T) {
	a := &Job{ID: "a", Platform: "p", ChatID: "c", Prompt: "one", LastSessionID: "s-a", entryID: 7}
	tbl := tableWith(t, a)

	got, ok := tbl.snapshot("a")
	if !ok || got.Prompt != "one" {
		t.Fatalf("snapshot(a) = %+v, %v", got, ok)
	}
	got.Prompt = "changed"
	chat := tbl.forChat(chatKeyFor("p", "c"))
	chat[0].Prompt = "changed"
	all, _ := tbl.allWithEntryIDs(nil)
	all[0].Job.Prompt = "changed"
	if a.Prompt != "one" {
		t.Errorf("a returned copy aliased the registered job: Prompt = %q", a.Prompt)
	}
	if _, ok := tbl.snapshot("missing"); ok {
		t.Error("snapshot of an unregistered id reported ok")
	}
}

func TestJobTableForChat(t *testing.T) {
	tbl := tableWith(t,
		&Job{ID: "a", Platform: "p", ChatID: "c"},
		&Job{ID: "b", Platform: "p", ChatID: "c"},
		&Job{ID: "x", Platform: "p", ChatID: "other"},
	)
	var ids []string
	for _, j := range tbl.forChat(chatKeyFor("p", "c")) {
		ids = append(ids, j.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"a", "b"}) {
		t.Errorf("forChat(p/c) = %v, want [a b]", ids)
	}
	if empty := tbl.forChat(chatKeyFor("p", "none")); empty == nil || len(empty) != 0 {
		t.Errorf("forChat of an empty chat = %#v, want a non-nil empty slice", empty)
	}
}

// allWithEntryIDs pairs each job with its entry ID at the same index, and
// reuses the caller's buffer when it is big enough.
func TestJobTableAllWithEntryIDs(t *testing.T) {
	tbl := tableWith(t,
		&Job{ID: "a", Platform: "p", ChatID: "c", entryID: 1},
		&Job{ID: "b", Platform: "p", ChatID: "c", entryID: 2},
		&Job{ID: "z", Platform: "p", ChatID: "c"},
	)
	buf := make([]cronEntryID, 0, 8)
	jobs, ids := tbl.allWithEntryIDs(buf)
	if len(jobs) != 3 || len(ids) != 3 {
		t.Fatalf("got %d jobs, %d ids; want 3 and 3", len(jobs), len(ids))
	}
	want := map[string]cronEntryID{"a": 1, "b": 2, "z": 0}
	for i, j := range jobs {
		if ids[i] != want[j.Job.ID] || !j.NextRun.IsZero() {
			t.Errorf("index %d: job %s paired with entry %d (NextRun %v), want entry %d", i, j.Job.ID, ids[i], j.NextRun, want[j.Job.ID])
		}
	}
	if &ids[:1][0] != &buf[:1][0] {
		t.Error("a large enough buffer was not reused")
	}
}

func TestJobTableSessionIDs(t *testing.T) {
	tbl := tableWith(t,
		&Job{ID: "a", Platform: "p", ChatID: "c", LastSessionID: "s-a"},
		&Job{ID: "b", Platform: "p", ChatID: "c"},
	)
	into := map[string]struct{}{}
	ids := tbl.sessionIDs([]string{"pre"}, into)
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"a", "b", "pre"}) {
		t.Errorf("ids = %v, want the prefix plus a and b", ids)
	}
	if _, ok := into["s-a"]; !ok || len(into) != 1 {
		t.Errorf("session ids = %v, want exactly s-a", into)
	}
}

// EnsureStub registers the stub from a copy of the job: its workspace, its
// prompt, and its last session as the chain.
func TestEnsureStub_RegistersFromTheJobCopy(t *testing.T) {
	r := &reapRouter{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: r})
	s.putJobForTest(&Job{ID: "jS", WorkDir: "/w", Prompt: "p", LastSessionID: "sess-1", Schedule: "0 * * * *"})
	if !s.EnsureStub("cron:jS") {
		t.Fatal("EnsureStub returned false for a registered job")
	}
	_, regs := r.snapshot()
	if len(regs) != 1 {
		t.Fatalf("got %d stub registrations, want 1", len(regs))
	}
	if got := regs[0]; got.key != "cron:jS" || got.workspace != "/w" || got.prompt != "p" || !slices.Equal(got.chainIDs, []string{"sess-1"}) {
		t.Errorf("registered %+v, want cron:jS /w p [sess-1]", got)
	}
}

// jobExists answers from the registry (the COR-001 TOCTOU re-check).
func TestJobExists(t *testing.T) {
	s := &Scheduler{tbl: newJobTable(nil)}
	s.tbl.mu.Lock()
	s.tbl.jobs["a"] = &Job{ID: "a"}
	s.tbl.mu.Unlock()
	if !s.jobExists("a") || s.jobExists("gone") {
		t.Errorf("jobExists(a)=%v jobExists(gone)=%v, want true and false", s.jobExists("a"), s.jobExists("gone"))
	}
}

// A job deleted between the dispatch CAS and the snapshot is not run: no
// snapshot, no started frame.
func TestExecSnapshotAndEmit_DeletedJobAborts(t *testing.T) {
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, AllowNilRouter: true}, SchedulerDeps{Telemetry: rec})
	_, _, _, abort := s.execSnapshotAndEmit("gone", true, "run-1", time.Now(), TriggerManual, &runInflight{})
	if !abort {
		t.Fatal("a deleted job was not aborted")
	}
	if n := rec.startedCount(); n != 0 {
		t.Errorf("%d started frames for an aborted run, want 0", n)
	}
}

// A replay whose job is gone by the time it snapshots gives the gate back.
func TestDispatchReplay_DeletedJobReleasesTheGate(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, AllowNilRouter: true}, SchedulerDeps{})
	if _, err := s.dispatchReplay("gone", "p", "m", "orig"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("dispatchReplay = %v, want ErrJobNotFound", err)
	}
	if _, won := s.gate.acquire("gone"); !won {
		t.Error("the gate was left held after the replay gave up")
	}
}
