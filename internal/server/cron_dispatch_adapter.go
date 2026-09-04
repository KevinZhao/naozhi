// cron_dispatch_adapter.go adapts the concrete cron scheduler surface onto
// dispatch.CronCommands, the projection-typed seam that keeps
// internal/dispatch free of an internal/cron import (#1164). server hosts it
// because it already imports both packages.
package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
)

// cronCommandScheduler is the slash-command subset of *cron.Scheduler that
// the dispatch adapter consumes. *cron.Scheduler satisfies it implicitly
// (pinned in cronview_contract_test.go).
type cronCommandScheduler interface {
	AddJob(j *cron.Job) error
	NextRun(j *cron.Job) time.Time
	ListJobs(plat, chatID string) []cron.Job
	DeleteJob(idPrefix, plat, chatID string) (*cron.Job, error)
	PauseJob(idPrefix, plat, chatID string) (*cron.Job, error)
	ResumeJob(idPrefix, plat, chatID string) (*cron.Job, error)
}

// cronDispatchAdapter implements dispatch.CronCommands over the concrete
// scheduler surface. Jobs cross as dispatch.CronJob projections
// (projectCronJob is the single copy site); errors are returned UNWRAPPED so
// the sentinel chain survives for ClassifyError; the dispatch-side CronCode*
// constants must match cron.ClassifyError wire values byte-for-byte.
//
// A nil scheduler must NOT be wrapped — Server.Start passes a genuinely nil
// dispatch.CronCommands, or the `d.scheduler != nil` "/cron disabled" gates
// would be defeated.
type cronDispatchAdapter struct{ s cronCommandScheduler }

// projectCronJob copies the dispatch-read fields (ID / Schedule / Prompt /
// Paused) into the dispatch-side projection. nil maps to the zero value so
// a scheduler that returns (nil, nil) cannot panic the adapter.
func projectCronJob(j *cron.Job) dispatch.CronJob {
	if j == nil {
		return dispatch.CronJob{}
	}
	return dispatch.CronJob{
		ID:       j.ID,
		Schedule: j.Schedule,
		Prompt:   j.Prompt,
		Paused:   j.Paused,
	}
}

// AddJob constructs the concrete job via cron.NewJob (the single construction
// choke point), registers it, and folds the follow-up NextRun into the result.
func (a cronDispatchAdapter) AddJob(req dispatch.CronJobRequest) (dispatch.CronJob, time.Time, error) {
	job := cron.NewJob(req.Schedule, req.Prompt, cron.JobIMContext{
		Platform:  req.Platform,
		ChatID:    req.ChatID,
		ChatType:  req.ChatType,
		CreatedBy: req.CreatedBy,
	})
	if err := a.s.AddJob(job); err != nil {
		// Unwrapped: ClassifyError must still see the sentinel chain.
		return dispatch.CronJob{}, time.Time{}, err
	}
	return projectCronJob(job), a.s.NextRun(job), nil
}

func (a cronDispatchAdapter) ListJobs(plat, chatID string) []dispatch.CronJob {
	jobs := a.s.ListJobs(plat, chatID)
	if len(jobs) == 0 {
		return nil
	}
	out := make([]dispatch.CronJob, len(jobs))
	for i := range jobs {
		out[i] = projectCronJob(&jobs[i])
	}
	return out
}

func (a cronDispatchAdapter) DeleteJob(idPrefix, plat, chatID string) (dispatch.CronJob, error) {
	j, err := a.s.DeleteJob(idPrefix, plat, chatID)
	if err != nil {
		return dispatch.CronJob{}, err
	}
	return projectCronJob(j), nil
}

func (a cronDispatchAdapter) PauseJob(idPrefix, plat, chatID string) (dispatch.CronJob, error) {
	j, err := a.s.PauseJob(idPrefix, plat, chatID)
	if err != nil {
		return dispatch.CronJob{}, err
	}
	return projectCronJob(j), nil
}

// ResumeJob folds the follow-up NextRun read into the return value, like AddJob.
func (a cronDispatchAdapter) ResumeJob(idPrefix, plat, chatID string) (dispatch.CronJob, time.Time, error) {
	j, err := a.s.ResumeJob(idPrefix, plat, chatID)
	if err != nil {
		return dispatch.CronJob{}, time.Time{}, err
	}
	return projectCronJob(j), a.s.NextRun(j), nil
}

func (a cronDispatchAdapter) ClassifyError(err error) string {
	return string(cron.ClassifyError(err))
}
