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
	setFreshErr    error
	setFreshPaused bool
	nextRunResult  time.Time
	// classifyResult is the wire code ClassifyError returns for any
	// non-nil error; "" falls back to "unknown" mirroring the real
	// classifier's CodeUnknown default for unmatched errors.
	classifyResult string

	lastAddReq     CronJobRequest
	addJobCalls    int
	listJobsCalls  int
	deleteJobCalls int
	setFreshCalls  int
	lastSetFresh   struct {
		idPrefix, plat, chatID string
		fresh                  bool
	}
}

func (f *fakeCronScheduler) AddJob(req CronJobRequest) (CronJob, time.Time, error) {
	f.addJobCalls++
	f.lastAddReq = req
	if f.addJobErr != nil {
		return CronJob{}, time.Time{}, f.addJobErr
	}
	return CronJob{ID: "fake-id", Schedule: req.Schedule, Prompt: req.Prompt, FreshContext: req.FreshContext}, f.nextRunResult, nil
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
	if f.pauseJobErr != nil {
		return CronJob{}, f.pauseJobErr
	}
	return CronJob{ID: idPrefix}, nil
}

func (f *fakeCronScheduler) ResumeJob(idPrefix, plat, chatID string) (CronJob, time.Time, error) {
	if f.resumeJobErr != nil {
		return CronJob{}, time.Time{}, f.resumeJobErr
	}
	return CronJob{ID: idPrefix}, f.nextRunResult, nil
}

func (f *fakeCronScheduler) SetFreshContext(idPrefix, plat, chatID string, fresh bool) (CronJob, error) {
	f.setFreshCalls++
	f.lastSetFresh.idPrefix, f.lastSetFresh.plat, f.lastSetFresh.chatID = idPrefix, plat, chatID
	f.lastSetFresh.fresh = fresh
	if f.setFreshErr != nil {
		return CronJob{}, f.setFreshErr
	}
	return CronJob{ID: idPrefix, FreshContext: fresh, Paused: f.setFreshPaused}, nil
}

func (f *fakeCronScheduler) ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	if f.classifyResult != "" {
		return f.classifyResult
	}
	return "unknown"
}

// handleCronAdd answers each AddJob failure class with its own reply: a full
// quota must not be reported as a malformed expression (#3009), and an
// unclassified failure must not blame the schedule either. Raw err.Error()
// must never appear in the reply. The code->class mapping itself is pinned
// by internal/cron/error_class_test.go.
func TestHandleCronAdd_ErrorReplyByClass(t *testing.T) {
	cases := []struct {
		name       string
		addJobErr  error
		classify   string
		wantSubstr string
		notSubstr  string
	}{
		{
			name:       "job_quota_exceeded",
			addJobErr:  errors.New("cron: job quota exceeded: per-chat cron limit reached (10)"),
			classify:   CronCodeJobQuotaExceeded,
			wantSubstr: "定时任务数已达上限",
			notSubstr:  "per-chat", // raw err.Error() must not leak
		},
		{
			name:       "unknown",
			addJobErr:  errors.New("opaque"),
			classify:   "", // fake falls back to "unknown"
			wantSubstr: "创建失败，请稍后再试",
			notSubstr:  "定时表达式",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fp := &fakePlatform{}
			d := newTestDispatcher(fp)
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

// /cron add and /cron resume print the next run with year and the
// scheduler's zone abbreviation, since cron.timezone need not be the reader's.
func TestCronNextRunShowsYearAndZone(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	next := time.Date(2026, time.October, 1, 9, 0, 0, 0, sh)
	const want = "Next: 2026-10-01 09:00 CST"

	for _, sub := range []string{"add", "resume"} {
		d := newTestDispatcher(&fakePlatform{})
		d.scheduler = &fakeCronScheduler{nextRunResult: next}
		var got string
		reply := func(s string) { got = s }
		if sub == "add" {
			d.handleCronAdd(incomingMsg(`/cron add "@every 30m" x`),
				[]string{"/cron", "add", `"@every 30m" x`}, reply, slog.Default())
		} else {
			d.handleCronResume(incomingMsg("/cron resume abc"),
				[]string{"/cron", "resume", "abc"}, reply, slog.Default())
		}
		if !strings.Contains(got, want) {
			t.Errorf("/cron %s reply = %q, want substring %q", sub, got, want)
		}
	}
	if got := formatCronNext(time.Time{}); got != "—" {
		t.Errorf("formatCronNext(zero) = %q, want \"—\"", got)
	}
}
