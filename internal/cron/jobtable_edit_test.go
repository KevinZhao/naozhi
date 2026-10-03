package cron

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestJobTableInsert(t *testing.T) {
	tbl, _, _ := mutateTable(t)
	active := &Job{Schedule: "@every 5m", Prompt: "go", WorkDir: "/w", Platform: "p", ChatID: "c3", LastSessionID: "s1"}
	r, err := tbl.insert(active, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID == "" || active.CreatedAt.IsZero() || tbl.jobs[active.ID] != active || !slices.Contains(tbl.sortedJobIDs, active.ID) {
		t.Fatalf("insert did not register %+v", active)
	}
	if r.plan == nil || r.plan.jobID != active.ID || r.snap.seq != 1 || !strings.Contains(string(r.snap.data), active.ID) {
		t.Errorf("insert result: plan=%+v seq=%d", r.plan, r.snap.seq)
	}
	if r.stub != (jobStubFields{id: active.ID, workDir: "/w", prompt: "go", lastSessionID: "s1"}) {
		t.Errorf("stub = %+v", r.stub)
	}
	paused := &Job{Schedule: "@every 5m", Platform: "p", ChatID: "c3", Paused: true}
	if r, err := tbl.insert(paused, 10, 10); err != nil || r.plan != nil {
		t.Errorf("paused insert: plan=%v err=%v, want no plan", r.plan, err)
	}
}

// Every refusal leaves the table as it was and takes no seq.
func TestJobTableInsert_Refusals(t *testing.T) {
	tbl, _, _ := mutateTable(t) // two jobs in p/c
	for _, tc := range []struct {
		name            string
		j               *Job
		maxJobs, perCht int
		want            string
	}{
		{"global cap", &Job{Schedule: "@every 5m", Platform: "p", ChatID: "new"}, 2, 10, "cron: job quota exceeded: max cron jobs reached (2)"},
		{"per-chat cap", &Job{Schedule: "@every 5m", Platform: "p", ChatID: "c"}, 10, 2, "cron: job quota exceeded: per-chat cron limit reached (2)"},
		{"bad schedule", &Job{Schedule: "never", Platform: "p", ChatID: "new"}, 10, 10, ""},
	} {
		if _, err := tbl.insert(tc.j, tc.maxJobs, tc.perCht); err == nil || (tc.want != "" && err.Error() != tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if len(tbl.jobs) != 2 || len(tbl.sortedJobIDs) != 2 || tbl.saveSeq.Load() != 0 {
		t.Errorf("a refused insert changed the table: %d jobs", len(tbl.jobs))
	}
	// Room under both caps is not a refusal.
	if _, err := tbl.insert(&Job{Schedule: "@every 5m", Platform: "p", ChatID: "c"}, 3, 3); err != nil {
		t.Errorf("insert at cap-1: %v", err)
	}
}

func TestJobTableInsert_PersistFailureUnregisters(t *testing.T) {
	tbl, _, _ := mutateTable(t)
	failMarshal(tbl)
	j := &Job{Schedule: "@every 5m", Platform: "p", ChatID: "c"}
	if _, err := tbl.insert(j, 10, 10); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("err = %v, want ErrPersistFailed", err)
	}
	if _, ok := tbl.jobs[j.ID]; ok || len(tbl.sortedJobIDs) != 2 || tbl.chatJobCount[chatKeyFor("p", "c")] != 2 {
		t.Error("a failed insert stayed registered")
	}
}

func TestJobTableUpdate(t *testing.T) {
	str := func(s string) *string { return &s }
	t.Run("reschedule active", func(t *testing.T) {
		tbl, active, _ := mutateTable(t)
		p, _ := planCronEntry(active.ID, active.Schedule, time.Now())
		active.cachedPeriod, active.cachedSched = p.period, p.sched
		r, err := tbl.update(active.ID, JobUpdate{Schedule: str("@every 1h"), Prompt: str("new")})
		if err != nil {
			t.Fatal(err)
		}
		want := reschedule{removeEntry: 7, oldSchedule: "@every 5m", newSchedule: "@every 1h"}
		if r.resched == nil || *r.resched != want {
			t.Fatalf("resched = %+v, want %+v", r.resched, want)
		}
		if active.Schedule != "@every 1h" || active.entryID != 0 || active.cachedPeriod != 0 || active.cachedSched != nil || r.job.Prompt != "new" || r.snap.seq != 1 {
			t.Errorf("after update: %+v, seq %d", active, r.snap.seq)
		}
	})
	t.Run("paused or unchanged schedule swaps nothing", func(t *testing.T) {
		tbl, active, paused := mutateTable(t)
		r, err := tbl.update(paused.ID, JobUpdate{Schedule: str("@every 1h")})
		if err != nil || r.resched != nil || paused.Schedule != "@every 1h" {
			t.Errorf("paused: resched=%+v schedule=%q err=%v", r.resched, paused.Schedule, err)
		}
		r, err = tbl.update(active.ID, JobUpdate{Schedule: str("@every 5m")})
		if err != nil || r.resched != nil || active.entryID != 7 {
			t.Errorf("unchanged: resched=%+v entryID=%d err=%v", r.resched, active.entryID, err)
		}
	})
	t.Run("failures restore the job", func(t *testing.T) {
		tbl, active, _ := mutateTable(t)
		before := *active
		if _, err := tbl.update(active.ID, JobUpdate{Placement: str("sandbox"), WorkDir: str("/w")}); !errors.Is(err, ErrSandboxWorkDir) {
			t.Errorf("sandbox+work_dir: err = %v", err)
		}
		failMarshal(tbl)
		if _, err := tbl.update(active.ID, JobUpdate{Schedule: str("@every 1h"), Prompt: str("x")}); !errors.Is(err, ErrPersistFailed) {
			t.Errorf("persist failure: err = %v", err)
		}
		if active.Schedule != before.Schedule || active.Prompt != before.Prompt || active.entryID != 7 || active.Placement != "" || active.WorkDir != "" {
			t.Errorf("not restored: %+v", active)
		}
		if _, err := tbl.update("nope", JobUpdate{}); !errors.Is(err, ErrJobNotFound) {
			t.Errorf("missing: err = %v", err)
		}
		if tbl.saveSeq.Load() != 0 {
			t.Error("a failed update took a seq")
		}
	})
}

func TestJobTableRevertSchedule(t *testing.T) {
	tbl, active, _ := mutateTable(t)
	active.Schedule = "@every 1h"
	snap, ok, err := tbl.revertSchedule(active.ID, "@every 5m", false)
	if !ok || err != nil || active.Schedule != "@every 5m" || active.Paused || snap.seq != 1 {
		t.Errorf("revert: ok=%v err=%v job=%+v seq=%d", ok, err, active, snap.seq)
	}
	if _, _, err := tbl.revertSchedule(active.ID, "bad", true); err != nil || !active.Paused {
		t.Errorf("revert with pause: paused=%v err=%v", active.Paused, err)
	}
	if _, ok, _ := tbl.revertSchedule("nope", "x", false); ok {
		t.Error("revert of a missing job reported ok")
	}
	failMarshal(tbl)
	if _, ok, err := tbl.revertSchedule(active.ID, "@every 5m", false); !ok || !errors.Is(err, ErrPersistFailed) {
		t.Errorf("persist failure: ok=%v err=%v", ok, err)
	}
}

func TestJobTableFillPrompt(t *testing.T) {
	tbl, active, paused := mutateTable(t)
	paused.WorkDir, paused.LastSessionID = "/w", "s1"
	r, err := tbl.fillPrompt(paused.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if paused.Prompt != "hello" || paused.Paused || r.plan == nil || r.plan.jobID != paused.ID || r.snap.seq != 1 {
		t.Errorf("fill paused: job=%+v plan=%+v", paused, r.plan)
	}
	if r.stub != (jobStubFields{id: paused.ID, workDir: "/w", prompt: "hello", lastSessionID: "s1"}) {
		t.Errorf("stub = %+v", r.stub)
	}
	if _, err := tbl.fillPrompt(paused.ID, "again"); !errors.Is(err, ErrPromptAlreadySet) || paused.Prompt != "hello" {
		t.Errorf("second fill: err=%v prompt=%q", err, paused.Prompt)
	}
	if r, err := tbl.fillPrompt(active.ID, "hi"); err != nil || r.plan != nil || active.Prompt != "hi" {
		t.Errorf("fill active: plan=%v err=%v", r.plan, err)
	}
	if _, err := tbl.fillPrompt("nope", "x"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("missing: err = %v", err)
	}
}

func TestJobTableFillPrompt_FailuresRestore(t *testing.T) {
	tbl, _, paused := mutateTable(t)
	paused.Schedule = "never"
	if _, err := tbl.fillPrompt(paused.ID, "x"); err == nil || paused.Prompt != "" || !paused.Paused {
		t.Errorf("bad schedule: err=%v job=%+v", err, paused)
	}
	paused.Schedule = "@every 5m"
	failMarshal(tbl)
	if _, err := tbl.fillPrompt(paused.ID, "x"); !errors.Is(err, ErrPersistFailed) || paused.Prompt != "" || !paused.Paused {
		t.Errorf("persist failure: err=%v job=%+v", err, paused)
	}
	if tbl.saveSeq.Load() != 0 {
		t.Error("a failed fill took a seq")
	}
}
