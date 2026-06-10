package server

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
)

// Compile-time contract (#1164): the adapter must satisfy the dispatch-side
// CronCommands seam, and *cron.Scheduler must satisfy the adapter's
// concrete-typed consumer subset. Either drifting fails the build here,
// local to the adapter that owns the cron↔dispatch translation.
var (
	_ dispatch.CronCommands = cronDispatchAdapter{}
	_ cronCommandScheduler  = (*cron.Scheduler)(nil)
)

// TestCronDispatchAdapter_WireCodesMatchCron pins the wire-value contract
// between dispatch's CronCode* constants and cron.ErrCode (#1164): dispatch
// compares CronCommands.ClassifyError output against its own string
// constants, and the adapter returns string(cron.ClassifyError(err)) — so
// the two constant sets must stay byte-identical or every /cron error reply
// silently degrades to the generic fallback.
func TestCronDispatchAdapter_WireCodesMatchCron(t *testing.T) {
	pairs := []struct {
		name     string
		dispatch string
		cron     cron.ErrCode
	}{
		{"job_not_found", dispatch.CronCodeJobNotFound, cron.CodeJobNotFound},
		{"ambiguous_prefix", dispatch.CronCodeAmbiguousPrefix, cron.CodeAmbiguousPrefix},
		{"job_already_paused", dispatch.CronCodeJobAlreadyPaused, cron.CodeJobAlreadyPaused},
		{"job_not_paused", dispatch.CronCodeJobNotPaused, cron.CodeJobNotPaused},
		{"invalid_prompt", dispatch.CronCodeInvalidPrompt, cron.CodeInvalidPrompt},
	}
	for _, p := range pairs {
		if p.dispatch != string(p.cron) {
			t.Errorf("%s: dispatch constant %q != cron wire value %q", p.name, p.dispatch, string(p.cron))
		}
	}
}

// TestCronDispatchAdapter_ClassifyError_PreservesSentinelChain verifies the
// adapter's no-wrap error contract end to end: a sentinel-wrapped scheduler
// error crossing the adapter must still classify to the matching wire code.
func TestCronDispatchAdapter_ClassifyError_PreservesSentinelChain(t *testing.T) {
	a := cronDispatchAdapter{}
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{cron.ErrJobNotFound, dispatch.CronCodeJobNotFound},
		{cron.ErrAmbiguousPrefix, dispatch.CronCodeAmbiguousPrefix},
		{cron.ErrJobAlreadyPaused, dispatch.CronCodeJobAlreadyPaused},
		{cron.ErrJobNotPaused, dispatch.CronCodeJobNotPaused},
		{cron.ErrInvalidPrompt, dispatch.CronCodeInvalidPrompt},
		{errors.New("opaque"), string(cron.CodeUnknown)},
	}
	for _, c := range cases {
		if got := a.ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestCronDispatchAdapter_ProjectCronJob pins the 4-field projection and its
// nil tolerance: a handler reading a field the projection does not copy
// would read a zero value, so adding a field to dispatch.CronJob must extend
// projectCronJob in the same change (cron_consumer.go godoc).
func TestCronDispatchAdapter_ProjectCronJob(t *testing.T) {
	j := &cron.Job{ID: "id1", Schedule: "@hourly", Prompt: "p", Paused: true}
	got := projectCronJob(j)
	want := dispatch.CronJob{ID: "id1", Schedule: "@hourly", Prompt: "p", Paused: true}
	if got != want {
		t.Errorf("projectCronJob = %+v, want %+v", got, want)
	}
	if zero := projectCronJob(nil); zero != (dispatch.CronJob{}) {
		t.Errorf("projectCronJob(nil) = %+v, want zero value", zero)
	}
}

// newAdapterTestScheduler stands up a real started *cron.Scheduler for
// integration-shaped adapter tests (the dispatch-side fake now covers the
// handler branches; the real AddJob/Delete/Pause/Resume round-trip moved
// here with #1164).
func newAdapterTestScheduler(t *testing.T) *cron.Scheduler {
	t.Helper()
	s := cron.NewScheduler(cron.SchedulerConfig{
		StorePath: filepath.Join(t.TempDir(), "cron_jobs.json"),
		MaxJobs:   10,
	}, cron.SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// TestCronDispatchAdapter_AddListResumeRoundTrip drives the adapter against
// a real scheduler: AddJob returns the projection plus a live next-run time
// (the AddJob/NextRun merge of #1164), ListJobs projects the stored job, and
// Pause→Resume round-trips with a fresh next-run time.
func TestCronDispatchAdapter_AddListResumeRoundTrip(t *testing.T) {
	a := cronDispatchAdapter{s: newAdapterTestScheduler(t)}

	job, next, err := a.AddJob(dispatch.CronJobRequest{
		Schedule: "@every 30m",
		Prompt:   "check status",
		Platform: "feishu",
		ChatID:   "c1",
		ChatType: "direct",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.ID == "" || job.Schedule != "@every 30m" || job.Prompt != "check status" {
		t.Fatalf("AddJob projection = %+v, want populated ID + echoed fields", job)
	}
	if next.IsZero() {
		t.Fatal("AddJob next-run time is zero; the NextRun fold must return the live schedule")
	}

	jobs := a.ListJobs("feishu", "c1")
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Paused {
		t.Fatalf("ListJobs = %+v, want the one un-paused job just added", jobs)
	}

	paused, err := a.PauseJob(job.ID, "feishu", "c1")
	if err != nil || !paused.Paused {
		t.Fatalf("PauseJob = (%+v, %v), want Paused=true", paused, err)
	}
	resumed, rnext, err := a.ResumeJob(job.ID, "feishu", "c1")
	if err != nil || resumed.Paused {
		t.Fatalf("ResumeJob = (%+v, %v), want Paused=false", resumed, err)
	}
	if rnext.IsZero() {
		t.Fatal("ResumeJob next-run time is zero; the NextRun fold must return the live schedule")
	}

	deleted, err := a.DeleteJob(job.ID, "feishu", "c1")
	if err != nil || deleted.ID != job.ID {
		t.Fatalf("DeleteJob = (%+v, %v), want deleted projection", deleted, err)
	}
	if remaining := a.ListJobs("feishu", "c1"); len(remaining) != 0 {
		t.Fatalf("ListJobs after delete = %+v, want empty", remaining)
	}
}

// TestCronDispatchAdapter_MutationErrorsClassify verifies a wrong-chat
// delete classifies as job_not_found across the adapter — i.e. the error
// returned by the real scheduler survives un-wrapped through the adapter's
// mutation methods into ClassifyError.
func TestCronDispatchAdapter_MutationErrorsClassify(t *testing.T) {
	a := cronDispatchAdapter{s: newAdapterTestScheduler(t)}
	_, err := a.DeleteJob("nosuchjob", "feishu", "c1")
	if err == nil {
		t.Fatal("expected DeleteJob error for unknown id")
	}
	if got := a.ClassifyError(err); got != dispatch.CronCodeJobNotFound {
		t.Errorf("ClassifyError = %q, want %q", got, dispatch.CronCodeJobNotFound)
	}
}
