// cron_dispatch_adapter.go adapts the concrete cron scheduler surface onto
// dispatch.CronCommands — the projection-typed seam that #1164 (R250-ARCH-1)
// introduced to cut the internal/dispatch → internal/cron import edge (RFC
// cron-sysession-merge §3.6). The server package is the natural host: it is
// the wiring point that already imports both cron and dispatch, and it holds
// the send_dispatch_adapter.go precedent (serverCaps) for thin shells that
// bind *Server-side concretions onto dispatch consumer interfaces.
package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
)

// cronCommandScheduler is the slash-command subset of *cron.Scheduler that
// the dispatch adapter consumes. It carries the concrete cron types the
// dispatch-side CronCommands seam deliberately no longer names (#1164).
// *cron.Scheduler satisfies it implicitly (pinned via cronScheduler in
// cronview_contract_test.go); the server's cronScheduler consumer aggregate
// embeds this so Server.scheduler keeps advertising exactly what is used.
type cronCommandScheduler interface {
	AddJob(j *cron.Job) error
	NextRun(j *cron.Job) time.Time
	ListJobs(plat, chatID string) []cron.Job
	DeleteJob(idPrefix, plat, chatID string) (*cron.Job, error)
	PauseJob(idPrefix, plat, chatID string) (*cron.Job, error)
	ResumeJob(idPrefix, plat, chatID string) (*cron.Job, error)
}

// cronDispatchAdapter implements dispatch.CronCommands over the concrete
// scheduler surface. Translation rules:
//
//   - Jobs cross the boundary as dispatch.CronJob projections (4 fields);
//     projectCronJob is the single copy site. The per-ListJobs projection
//     alloc is O(jobs-per-chat) ≤ MaxJobsPerChat on a non-hot IM command
//     path — acceptable.
//   - Errors are returned UNWRAPPED so the scheduler's sentinel chain
//     survives for ClassifyError (dispatch.CronCommands contract).
//   - ClassifyError returns string(cron.ClassifyError(err)); the dispatch-
//     side CronCode* constants must match those wire values byte-for-byte,
//     pinned by cron_dispatch_adapter_test.go.
//
// Wiring note: a nil scheduler must NOT be wrapped — Server.Start passes a
// genuinely nil dispatch.CronCommands when s.scheduler is nil, because a
// non-nil adapter value would defeat NewDispatcher's nil collapse and the
// `d.scheduler != nil` "/cron disabled" gates. #1164.
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

// AddJob constructs the concrete job (cron.NewJob keeps the single
// construction choke point of R250-CR-9), registers it, and folds the
// follow-up NextRun read into the return value — handleCronAdd was the only
// NextRun caller on the create path and invoked it immediately after AddJob,
// so the merge is semantics-preserving. #1164.
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

// ResumeJob folds the follow-up NextRun read into the return value, same
// rationale as AddJob (handleCronResume was the only other NextRun caller).
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
