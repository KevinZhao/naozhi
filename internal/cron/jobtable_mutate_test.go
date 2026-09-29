package cron

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failMarshal makes tbl's persist snapshot fail.
func failMarshal(tbl *jobTable) {
	fn := marshalJobsFn(func(any) ([]byte, error) { return nil, errors.New("disk says no") })
	var p atomic.Pointer[marshalJobsFn]
	p.Store(&fn)
	tbl.marshal = &p
}

func mutateTable(t *testing.T) (*jobTable, *Job, *Job) {
	t.Helper()
	active := &Job{ID: "aaaa1111", Schedule: "@every 5m", Platform: "p", ChatID: "c", entryID: 7}
	paused := &Job{ID: "bbbb2222", Schedule: "@every 5m", Platform: "p", ChatID: "c", Paused: true}
	return tableWith(t, active, paused), active, paused
}

func TestMutate_Delete(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	r := tbl.mutateByID(active.ID, mutDelete)
	if r.lookupErr != nil || r.opErr != nil || r.persistErr != nil {
		t.Fatalf("delete: %+v", r)
	}
	if r.removeEntry != 7 || r.plan != nil || r.job.ID != active.ID || r.seq != 1 {
		t.Errorf("delete result: removeEntry=%d plan=%v job=%q seq=%d", r.removeEntry, r.plan, r.job.ID, r.seq)
	}
	if _, ok := tbl.jobs[active.ID]; ok || slices.Contains(tbl.sortedJobIDs, active.ID) || tbl.chatJobCount[chatKeyFor("p", "c")] != 1 {
		t.Error("deleted job still indexed")
	}
	if strings.Contains(string(r.data), active.ID) || !strings.Contains(string(r.data), "bbbb2222") {
		t.Errorf("snapshot = %s, want only the remaining job", r.data)
	}
}

func TestMutate_Pause(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	r := tbl.mutateByID(active.ID, mutPause)
	if r.opErr != nil || r.persistErr != nil || r.removeEntry != 7 || r.plan != nil || r.seq != 1 {
		t.Fatalf("pause: %+v", r)
	}
	if !active.Paused || active.entryID != 0 || !r.job.Paused {
		t.Errorf("after pause: live Paused=%v entryID=%d, result Paused=%v", active.Paused, active.entryID, r.job.Paused)
	}
	if !strings.Contains(string(r.data), `"paused":true`) {
		t.Errorf("snapshot missed the pause: %s", r.data)
	}
	if _, ok := tbl.jobs[active.ID]; !ok {
		t.Error("pause dropped the job")
	}
}

func TestMutate_Resume(t *testing.T) {
	tbl, _, paused := mutateTable(t)
	r := tbl.mutateByID(paused.ID, mutResume)
	if r.opErr != nil || r.persistErr != nil || r.removeEntry != 0 || r.seq != 1 {
		t.Fatalf("resume: %+v", r)
	}
	if r.plan == nil || r.plan.jobID != paused.ID || r.plan.sched == nil {
		t.Fatalf("resume plan = %+v", r.plan)
	}
	if paused.Paused || r.job.Paused {
		t.Error("resume left the job paused")
	}
}

// A transition that does not apply, or a missing job, changes nothing and
// takes no seq.
func TestMutate_NoOps(t *testing.T) {
	tbl, active, paused := mutateTable(t)
	paused.Schedule = "not a schedule"
	for _, tc := range []struct {
		name      string
		r         mutationResult
		lookup    bool
		wantErrIs error
	}{
		{"missing", tbl.mutateByID("nope", mutDelete), true, ErrJobNotFound},
		{"pause paused", tbl.mutateByID(paused.ID, mutPause), false, ErrJobAlreadyPaused},
		{"resume active", tbl.mutateByID(active.ID, mutResume), false, ErrJobNotPaused},
		{"resume bad schedule", tbl.mutateByID(paused.ID, mutResume), false, nil},
	} {
		err := tc.r.opErr
		if tc.lookup {
			err = tc.r.lookupErr
		}
		if err == nil || (tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs)) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErrIs)
		}
		if tc.r.data != nil || tc.r.seq != 0 || tc.r.removeEntry != 0 || tc.r.plan != nil {
			t.Errorf("%s: result carries effects: %+v", tc.name, tc.r)
		}
	}
	if active.Paused || active.entryID != 7 || !paused.Paused || len(tbl.jobs) != 2 || tbl.saveSeq.Load() != 0 {
		t.Error("a no-op mutation changed the table")
	}
}

// A failed snapshot undoes a pause or resume before the lock is released, so
// no side effect runs for it; a delete stays done and keeps its entry to
// retire (#1149, #1272).
func TestMutate_PersistFailure(t *testing.T) {
	for _, kind := range []mutationKind{mutDelete, mutPause, mutResume} {
		tbl, active, paused := mutateTable(t)
		failMarshal(tbl)
		target := active
		if kind == mutResume {
			target = paused
		}
		r := tbl.mutateByID(target.ID, kind)
		if !errors.Is(r.persistErr, ErrPersistFailed) || r.data != nil || r.seq != 0 || tbl.saveSeq.Load() != 0 {
			t.Fatalf("kind %d: %+v", kind, r)
		}
		if kind == mutDelete {
			if r.removeEntry != 7 {
				t.Errorf("delete: removeEntry=%d, want 7", r.removeEntry)
			}
			if _, ok := tbl.jobs[target.ID]; ok {
				t.Error("delete was undone")
			}
			continue
		}
		if r.removeEntry != 0 || r.plan != nil {
			t.Errorf("kind %d: removeEntry=%d plan=%v", kind, r.removeEntry, r.plan)
		}
		if active.Paused || active.entryID != 7 || !paused.Paused || r.job.Paused != target.Paused {
			t.Errorf("kind %d: not rolled back: active=%v/%d paused=%v result=%v", kind, active.Paused, active.entryID, paused.Paused, r.job.Paused)
		}
	}
}

// The prefix form is scoped to one chat and refuses an ambiguous prefix.
func TestMutateByPrefix(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	other := &Job{ID: "aaaa9999", Schedule: "@every 5m", Platform: "p", ChatID: "c2"}
	tbl.jobs[other.ID] = other
	tbl.indexLocked(other)

	if r := tbl.mutateByPrefix("aaaa", "p", "c", mutPause); r.lookupErr != nil || r.job.ID != active.ID {
		t.Errorf("scoped prefix: %+v", r)
	}
	if r := tbl.mutateByPrefix("aaaa9", "p", "c", mutPause); !errors.Is(r.lookupErr, ErrJobNotFound) || other.Paused {
		t.Errorf("another chat's job: lookupErr=%v paused=%v", r.lookupErr, other.Paused)
	}
	third := &Job{ID: "aaaa3333", Schedule: "@every 5m", Platform: "p", ChatID: "c"}
	tbl.jobs[third.ID] = third
	tbl.indexLocked(third)
	if r := tbl.mutateByPrefix("aaaa", "p", "c", mutDelete); !errors.Is(r.lookupErr, ErrAmbiguousPrefix) || len(tbl.jobs) != 4 {
		t.Errorf("ambiguous: lookupErr=%v jobs=%d", r.lookupErr, len(tbl.jobs))
	}
}

// applyEntry writes a committed registration onto a registered job and
// reports a vanished one, whose freshly committed entry the caller must
// retire (the Stop teardown race).
func TestJobTableApplyEntry(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	p, err := planCronEntry(active.ID, "@every 5m", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !tbl.applyEntry(p, 42) || active.entryID != 42 || active.cachedSched == nil || active.cachedPeriod != 5*time.Minute {
		t.Errorf("applyEntry(registered) left entryID=%d sched=%v period=%v", active.entryID, active.cachedSched, active.cachedPeriod)
	}
	p.jobID = "gone"
	if tbl.applyEntry(p, 43) {
		t.Error("applyEntry reported an unregistered job as live")
	}
}

func TestCommitAndApplyCronEntry_RetiresOrphan(t *testing.T) {
	s, _ := newTestSchedulerForPersist(t)
	before := len(s.cron.Entries())
	p, err := planCronEntry("gone", "@every 1h", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.commitAndApplyCronEntry(p)
	if n := len(s.cron.Entries()); n != before {
		t.Errorf("entries = %d after committing for an unregistered job, want %d", n, before)
	}
}

// Each successful delete, pause and resume reaches cron_jobs.json, by ID and
// by prefix alike.
func TestMutations_Persist(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	onDisk := func(step string) *Job {
		t.Helper()
		jobs, err := loadJobs(s.storePath)
		if err != nil {
			t.Fatalf("%s: loadJobs: %v", step, err)
		}
		return jobs[id]
	}
	prefix := id[:6]
	steps := []struct {
		name       string
		do         func() error
		wantPaused bool
	}{
		{"ResumeJobByID", func() error { _, err := s.ResumeJobByID(id); return err }, false},
		{"PauseJobByID", func() error { _, err := s.PauseJobByID(id); return err }, true},
		{"ResumeJob", func() error { _, err := s.ResumeJob(prefix, "feishu", "chat1"); return err }, false},
		{"PauseJob", func() error { _, err := s.PauseJob(prefix, "feishu", "chat1"); return err }, true},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if j := onDisk(st.name); j == nil || j.Paused != st.wantPaused {
			t.Fatalf("%s: on disk %+v, want paused=%v", st.name, j, st.wantPaused)
		}
	}
	if _, err := s.DeleteJob(prefix, "feishu", "chat1"); err != nil {
		t.Fatal(err)
	}
	if j := onDisk("DeleteJob"); j != nil {
		t.Fatalf("DeleteJob: job still on disk")
	}

	j2 := &Job{Schedule: "@every 1h", Prompt: "x", Platform: "feishu", ChatID: "chat1", ChatType: "direct", Paused: true}
	if err := s.AddJob(j2); err != nil {
		t.Fatal(err)
	}
	id = j2.ID
	if onDisk("AddJob") == nil {
		t.Fatal("AddJob did not persist")
	}
	if _, err := s.DeleteJobByID(id); err != nil {
		t.Fatal(err)
	}
	if onDisk("DeleteJobByID") != nil {
		t.Fatal("DeleteJobByID: job still on disk")
	}
}
