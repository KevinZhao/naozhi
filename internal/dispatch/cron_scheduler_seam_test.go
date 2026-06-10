package dispatch

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeCronScheduler is a minimal CronCommands test seam, introduced as the
// CronScheduler fake by R250-ARCH-17 (#1178) and migrated to the
// projection-typed CronCommands interface by R250-ARCH-1 (#1164) — the
// fixture now uses dispatch.CronJob so this file no longer imports
// internal/cron. It captures the calls dispatch makes during slash-command
// handling and lets each test choose a return value without standing up a
// real cron.Scheduler (with its tempdir, persist loop, and robfig parse
// harness).
type fakeCronScheduler struct {
	addJobErr      error
	listJobsResult []CronJob
	deleteJobErr   error
	pauseJobErr    error
	resumeJobErr   error
	nextRunResult  time.Time
	// classifyResult is the wire code ClassifyError returns for any
	// non-nil error; "" falls back to "unknown" mirroring the real
	// classifier's CodeUnknown default for unmatched errors.
	classifyResult string

	addJobCalls    int
	listJobsCalls  int
	deleteJobCalls int
	pauseJobCalls  int
	resumeJobCalls int
	classifyCalls  int
}

func (f *fakeCronScheduler) AddJob(req CronJobRequest) (CronJob, time.Time, error) {
	f.addJobCalls++
	if f.addJobErr != nil {
		return CronJob{}, time.Time{}, f.addJobErr
	}
	return CronJob{ID: "fake-id", Schedule: req.Schedule, Prompt: req.Prompt}, f.nextRunResult, nil
}

func (f *fakeCronScheduler) ListJobs(plat, chatID string) []CronJob {
	f.listJobsCalls++
	return f.listJobsResult
}

func (f *fakeCronScheduler) DeleteJob(idPrefix, plat, chatID string) (CronJob, error) {
	f.deleteJobCalls++
	if f.deleteJobErr != nil {
		return CronJob{}, f.deleteJobErr
	}
	return CronJob{ID: idPrefix}, nil
}

func (f *fakeCronScheduler) PauseJob(idPrefix, plat, chatID string) (CronJob, error) {
	f.pauseJobCalls++
	if f.pauseJobErr != nil {
		return CronJob{}, f.pauseJobErr
	}
	return CronJob{ID: idPrefix}, nil
}

func (f *fakeCronScheduler) ResumeJob(idPrefix, plat, chatID string) (CronJob, time.Time, error) {
	f.resumeJobCalls++
	if f.resumeJobErr != nil {
		return CronJob{}, time.Time{}, f.resumeJobErr
	}
	return CronJob{ID: idPrefix}, f.nextRunResult, nil
}

func (f *fakeCronScheduler) ClassifyError(err error) string {
	f.classifyCalls++
	if err == nil {
		return ""
	}
	if f.classifyResult != "" {
		return f.classifyResult
	}
	return "unknown"
}

// TestCronCommands_Interface_SatisfiedByFake verifies fakeCronScheduler
// implements CronCommands — guards against silent drift if the interface
// adds a method but the fake forgets to mirror it. The production-side
// satisfaction check (cronDispatchAdapter implements CronCommands) lives
// with the adapter in internal/server/cron_dispatch_adapter_test.go, since
// #1164 removed the concrete *cron.Scheduler from dispatch's view entirely.
func TestCronCommands_Interface_SatisfiedByFake(t *testing.T) {
	var _ CronCommands = (*fakeCronScheduler)(nil)
}

// TestFakeCronScheduler_RecordsCalls exercises the fake's call-counting
// shape. Slash-command handlers route through these methods, and any
// future test that wants to assert "AddJob was called once" or "list
// returned an empty slice" can plug this fake in via Dispatcher.scheduler
// without building a real Scheduler.
func TestFakeCronScheduler_RecordsCalls(t *testing.T) {
	fake := &fakeCronScheduler{
		addJobErr:      errors.New("add failed"),
		listJobsResult: []CronJob{{ID: "abc", Schedule: "@hourly"}},
		nextRunResult:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	// AddJob: the fake-injected error reaches the caller verbatim.
	if _, _, err := fake.AddJob(CronJobRequest{}); err == nil {
		t.Fatal("expected injected AddJob error")
	}
	if fake.addJobCalls != 1 {
		t.Errorf("AddJob calls = %d, want 1", fake.addJobCalls)
	}

	// AddJob happy path: projection echoes the request and carries the
	// configured next-run time (the AddJob/NextRun merge of #1164).
	fake.addJobErr = nil
	job, next, err := fake.AddJob(CronJobRequest{Schedule: "@hourly", Prompt: "p"})
	if err != nil || job.Schedule != "@hourly" || job.Prompt != "p" {
		t.Errorf("AddJob happy-path: got (%+v, %v), want schedule/prompt echoed", job, err)
	}
	if !next.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("AddJob next = %v, want 2026-01-01", next)
	}

	// ListJobs: returned slice flows through unchanged.
	got := fake.ListJobs("feishu", "chatX")
	if len(got) != 1 || got[0].ID != "abc" {
		t.Errorf("ListJobs result = %v, want [{ID:abc Schedule:@hourly}]", got)
	}
	if fake.listJobsCalls != 1 {
		t.Errorf("ListJobs calls = %d, want 1", fake.listJobsCalls)
	}

	// Delete / Pause / Resume: all return the prefix as ID with nil err
	// on the happy path; Resume also carries the next-run time.
	if j, err := fake.DeleteJob("xyz", "feishu", "chatX"); err != nil || j.ID != "xyz" {
		t.Errorf("DeleteJob happy-path: got (%+v, %v), want ({ID:xyz}, nil)", j, err)
	}
	if j, err := fake.PauseJob("xyz", "feishu", "chatX"); err != nil || j.ID != "xyz" {
		t.Errorf("PauseJob happy-path: got (%+v, %v), want ({ID:xyz}, nil)", j, err)
	}
	if j, next, err := fake.ResumeJob("xyz", "feishu", "chatX"); err != nil || j.ID != "xyz" || next.IsZero() {
		t.Errorf("ResumeJob happy-path: got (%+v, %v, %v), want ({ID:xyz}, non-zero, nil)", j, next, err)
	}

	// ClassifyError: nil maps to the CodeOK-equivalent empty string,
	// unmatched non-nil errors fall back to "unknown".
	if code := fake.ClassifyError(nil); code != "" {
		t.Errorf("ClassifyError(nil) = %q, want \"\"", code)
	}
	if code := fake.ClassifyError(errors.New("boom")); code != "unknown" {
		t.Errorf("ClassifyError(err) = %q, want \"unknown\"", code)
	}
}

// TestHandleCronAdd_PromptVsScheduleErrorReply pins the #631-adjacent /
// R20260531-ARCH-2 fix on the create path: handleCronAdd must not collapse a
// prompt-policy rejection into the "请检查定时表达式格式" schedule message.
// A user whose schedule parsed fine but whose prompt was rejected
// (ClassifyError → CronCodeInvalidPrompt) gets a prompt-specific hint;
// every other AddJob failure keeps the generic schedule message. Raw
// err.Error() must never appear in the reply.
func TestHandleCronAdd_PromptVsScheduleErrorReply(t *testing.T) {
	cases := []struct {
		name       string
		addJobErr  error
		classify   string
		wantSubstr string
		notSubstr  string
	}{
		{
			name:       "invalid_prompt",
			addJobErr:  errors.New("wrapped: invalid prompt"),
			classify:   CronCodeInvalidPrompt,
			wantSubstr: "任务内容不合法",
			notSubstr:  "定时表达式",
		},
		{
			name:       "capacity_or_schedule_falls_back",
			addJobErr:  errors.New("per-chat cron limit reached (10)"),
			classify:   "", // fake falls back to "unknown"
			wantSubstr: "请检查定时表达式格式",
			notSubstr:  "per-chat", // raw err.Error() must not leak
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fp := &fakePlatform{}
			d := newTestDispatcher(fp, nil)
			d.scheduler = &fakeCronScheduler{addJobErr: c.addJobErr, classifyResult: c.classify}

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
