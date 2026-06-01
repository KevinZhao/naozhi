package dispatch

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// TestHandleCronAdd_PromptVsScheduleErrorReply pins the #631-adjacent /
// R20260531-ARCH-2 fix on the create path: handleCronAdd must not collapse a
// prompt-policy rejection into the "请检查定时表达式格式" schedule message.
// A user whose schedule parsed fine but whose prompt was rejected
// (ValidatePromptStrict → ErrInvalidPrompt) gets a prompt-specific hint;
// every other AddJob failure keeps the generic schedule message. Raw
// err.Error() must never appear in the reply.
func TestHandleCronAdd_PromptVsScheduleErrorReply(t *testing.T) {
	cases := []struct {
		name       string
		addJobErr  error
		wantSubstr string
		notSubstr  string
	}{
		{
			name:       "invalid_prompt",
			addJobErr:  fmt.Errorf("wrapped: %w", cron.ErrInvalidPrompt),
			wantSubstr: "任务内容不合法",
			notSubstr:  "定时表达式",
		},
		{
			name:       "capacity_or_schedule_falls_back",
			addJobErr:  errors.New("per-chat cron limit reached (10)"),
			wantSubstr: "请检查定时表达式格式",
			notSubstr:  "per-chat", // raw err.Error() must not leak
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fp := &fakePlatform{}
			d := newTestDispatcher(fp, nil)
			d.scheduler = &fakeCronScheduler{addJobErr: c.addJobErr}

			var got string
			reply := func(s string) { got = s }
			d.handleCronAdd(
				incomingMsg(`/cron add "@every 30m" do something`),
				[]string{"/cron", "add", `"@every 30m" do something`},
				reply,
				slog.Default(),
			)
			if !strings.Contains(got, c.wantSubstr) {
				t.Errorf("reply = %q, want substring %q", got, c.wantSubstr)
			}
			if c.notSubstr != "" && strings.Contains(got, c.notSubstr) {
				t.Errorf("reply = %q must NOT contain %q", got, c.notSubstr)
			}
		})
	}
}
