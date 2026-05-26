package dispatch

import (
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
)

// fakeCronScheduler is a minimal CronScheduler test seam introduced by
// R250-ARCH-17 (#1178). It captures the calls dispatch makes during
// slash-command handling and lets each test choose a return value
// without standing up a real cron.Scheduler (with its tempdir, persist
// loop, and robfig parse harness).
type fakeCronScheduler struct {
	addJobErr      error
	listJobsResult []cron.Job
	deleteJobErr   error
	pauseJobErr    error
	resumeJobErr   error
	nextRunResult  time.Time

	addJobCalls    int
	listJobsCalls  int
	deleteJobCalls int
	pauseJobCalls  int
	resumeJobCalls int
	nextRunCalls   int
}

func (f *fakeCronScheduler) AddJob(j *cron.Job) error {
	f.addJobCalls++
	return f.addJobErr
}

func (f *fakeCronScheduler) NextRun(j *cron.Job) time.Time {
	f.nextRunCalls++
	return f.nextRunResult
}

func (f *fakeCronScheduler) ListJobs(plat, chatID string) []cron.Job {
	f.listJobsCalls++
	return f.listJobsResult
}

func (f *fakeCronScheduler) DeleteJob(idPrefix, plat, chatID string) (*cron.Job, error) {
	f.deleteJobCalls++
	if f.deleteJobErr != nil {
		return nil, f.deleteJobErr
	}
	return &cron.Job{ID: idPrefix}, nil
}

func (f *fakeCronScheduler) PauseJob(idPrefix, plat, chatID string) (*cron.Job, error) {
	f.pauseJobCalls++
	if f.pauseJobErr != nil {
		return nil, f.pauseJobErr
	}
	return &cron.Job{ID: idPrefix}, nil
}

func (f *fakeCronScheduler) ResumeJob(idPrefix, plat, chatID string) (*cron.Job, error) {
	f.resumeJobCalls++
	if f.resumeJobErr != nil {
		return nil, f.resumeJobErr
	}
	return &cron.Job{ID: idPrefix}, nil
}

// TestCronScheduler_Interface_SatisfiedByConcreteScheduler pins the
// implicit-satisfaction contract that R250-ARCH-17 (#1178) relies on.
// If a future cron.Scheduler refactor renames or removes any of the six
// methods on the CronScheduler interface, this test fails at compile
// time — no runtime cost, but caught by `go test` not just `go vet`.
//
// The compile-time assertion lives inside a test function (rather than
// as a top-level _ = (CronScheduler)((*cron.Scheduler)(nil)) global)
// so a build break surfaces with the test name in the failure log,
// making the regression easier to bisect.
func TestCronScheduler_Interface_SatisfiedByConcreteScheduler(t *testing.T) {
	// Compile-time check. The right-hand side is a nil *cron.Scheduler
	// typed pointer; if Scheduler stops satisfying CronScheduler, the
	// build fails with a clear "wrong type" message at this line.
	var _ CronScheduler = (*cron.Scheduler)(nil)
}

// TestCronScheduler_Interface_SatisfiedByFake verifies fakeCronScheduler
// itself implements the interface — guards against silent drift if the
// interface adds a method but the fake forgets to mirror it.
func TestCronScheduler_Interface_SatisfiedByFake(t *testing.T) {
	var _ CronScheduler = (*fakeCronScheduler)(nil)
}

// TestFakeCronScheduler_RecordsCalls exercises the fake's call-counting
// shape. Slash-command handlers route through these methods, and any
// future test that wants to assert "AddJob was called once" or "list
// returned an empty slice" can plug this fake in via Dispatcher.scheduler
// without building a real Scheduler.
func TestFakeCronScheduler_RecordsCalls(t *testing.T) {
	fake := &fakeCronScheduler{
		addJobErr:      errors.New("add failed"),
		listJobsResult: []cron.Job{{ID: "abc", Schedule: "@hourly"}},
		nextRunResult:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	// AddJob: the fake-injected error reaches the caller verbatim.
	if err := fake.AddJob(&cron.Job{}); err == nil {
		t.Fatal("expected injected AddJob error")
	}
	if fake.addJobCalls != 1 {
		t.Errorf("AddJob calls = %d, want 1", fake.addJobCalls)
	}

	// ListJobs: returned slice flows through unchanged.
	got := fake.ListJobs("feishu", "chatX")
	if len(got) != 1 || got[0].ID != "abc" {
		t.Errorf("ListJobs result = %v, want [{ID:abc Schedule:@hourly}]", got)
	}
	if fake.listJobsCalls != 1 {
		t.Errorf("ListJobs calls = %d, want 1", fake.listJobsCalls)
	}

	// NextRun: configured time round-trips.
	if ts := fake.NextRun(&cron.Job{}); !ts.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("NextRun = %v, want 2026-01-01", ts)
	}

	// Delete / Pause / Resume: all return the prefix as ID with nil err
	// on the happy path.
	if j, err := fake.DeleteJob("xyz", "feishu", "chatX"); err != nil || j.ID != "xyz" {
		t.Errorf("DeleteJob happy-path: got (%v, %v), want (&{ID:xyz}, nil)", j, err)
	}
	if j, err := fake.PauseJob("xyz", "feishu", "chatX"); err != nil || j.ID != "xyz" {
		t.Errorf("PauseJob happy-path: got (%v, %v), want (&{ID:xyz}, nil)", j, err)
	}
	if j, err := fake.ResumeJob("xyz", "feishu", "chatX"); err != nil || j.ID != "xyz" {
		t.Errorf("ResumeJob happy-path: got (%v, %v), want (&{ID:xyz}, nil)", j, err)
	}
}
