package cron

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

// TestRunHistoryReader_SchedulerSatisfies pins R250-ARCH-9 (#1172): *Scheduler
// satisfies the narrow read-only RunHistoryReader interface so dashboard
// handlers that only read run history can depend on it instead of the whole
// *Scheduler. The compile-time `var _ RunHistoryReader = (*Scheduler)(nil)`
// in scheduler_finish.go already enforces method-set membership; this test
// additionally proves reads flow correctly when used through the interface.
func TestRunHistoryReader_SchedulerSatisfies(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 5}, SchedulerDeps{})

	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	s.runs.Append(run)

	// Use ONLY the narrow interface from here on.
	var r RunHistoryReader = s

	got, err := r.Run(jobID, run.RunID)
	if err != nil {
		t.Fatalf("GetRun via RunHistoryReader: %v", err)
	}
	if got == nil || got.RunID != run.RunID {
		t.Fatalf("GetRun returned %+v, want RunID %q", got, run.RunID)
	}

	list := r.ListRuns(jobID, 10, time.Time{})
	if len(list) != 1 || list[0].RunID != run.RunID {
		t.Fatalf("ListRuns via interface = %+v, want 1 entry %q", list, run.RunID)
	}

	recent := r.RecentRuns(jobID, 10)
	if len(recent) != 1 || recent[0].RunID != run.RunID {
		t.Fatalf("RecentRuns via interface = %+v, want 1 entry %q", recent, run.RunID)
	}

	if _, ok := r.CurrentRun(jobID); ok {
		t.Fatal("CurrentRun via interface should report no inflight run for an idle job")
	}
}

// TestRunHistoryReader_MissingRun pins the not-found path through the interface
// so handlers retyped to RunHistoryReader keep the 404 mapping.
func TestRunHistoryReader_MissingRun(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cron_jobs.json")
	var r RunHistoryReader = NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 5}, SchedulerDeps{})

	_, err := r.Run(mustGenerateID(), mustGenerateRunID())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("GetRun for missing run = %v, want fs.ErrNotExist", err)
	}
}

// makeRun builds a non-zero CronRun with a unique RunID under the given JobID.
// Caller can override fields after the helper returns.
func makeRun(jobID string, startedAt time.Time) *CronRun {
	return &CronRun{
		RunID:      mustGenerateRunID(),
		JobID:      jobID,
		State:      RunStateSucceeded,
		Trigger:    TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    startedAt.Add(time.Second),
		DurationMS: 1000,
		Prompt:     "hello",
		WorkDir:    "/tmp/wd",
		Result:     "ok",
	}
}
