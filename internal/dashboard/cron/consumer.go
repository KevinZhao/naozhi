// consumer.go — the consumer-side dependency interface this package needs
// (#2561).
//
// Deps named *cron.Scheduler, whose exported methods cover scheduling,
// persistence and the notify plumbing. SchedulerView is the MEASURED call
// surface — grep h.deps.Scheduler.<Method> across the package: this package IS
// the cron UI, so it reads jobs, runs and the inflight view, and it writes
// every job mutation the dashboard offers. What the interface buys is that
// the rest — the tick loop, the store internals, the notify senders — can change
// without touching the handlers, and that a test can fake the surface instead of
// standing up a real Scheduler.
//
// Declared here rather than in internal/dashboard/contracts: contracts is for
// shapes several sub-packages share, and only this package consumes a Scheduler
// at this width (dashsession takes CronView, 1 method).
//
// CAUTION: h.deps.Scheduler is nil-guarded in 16 places — a nil Scheduler is the
// documented "cron disabled" state. An interface field handed a nil CONCRETE
// pointer makes every one of those guards read true. The wiring site unwraps
// typed nils; TestCronHandlers_NilSchedulerStaysNilInterface pins it. Same class
// as #377, #2551 and dashsession's conversion earlier in #2561.
package cron

import (
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// SchedulerView is the *cron.Scheduler surface the cron dashboard uses: one
// Deps field, composed of one interface per purpose (#2897 D6), so a helper
// or a fake that needs one purpose names only that one.
type SchedulerView interface {
	JobStore
	RunHistory
	ScheduleFacts
}

// JobStore is job CRUD reachable from the cron panel.
type JobStore interface {
	AddJob(j *cronpkg.Job) error
	GetJob(id string) (cronpkg.Job, bool)
	ListJobs(plat, chatID string) []cronpkg.Job
	ListAllJobsWithNextRun() []cronpkg.JobWithNextRun
	UpdateJob(id string, upd cronpkg.JobUpdate) (*cronpkg.Job, error)
	DeleteJobByID(id string) (*cronpkg.Job, error)
	PauseJobByID(id string) (*cronpkg.Job, error)
	ResumeJobByID(id string) (*cronpkg.Job, error)
	TriggerNow(id string) error
}

// RunHistory is run history and the live run.
type RunHistory interface {
	Run(jobID, runID string) (*cronpkg.CronRun, error)
	ListRuns(jobID string, limit int, before time.Time) []cronpkg.CronRunSummary
	RecentRuns(jobID string, n int) []cronpkg.CronRunSummary
	CurrentRun(jobID string) (cronpkg.RunInflightView, bool)
}

// ScheduleFacts is the facts the payloads carry (timezone, notify default, start time).
type ScheduleFacts interface {
	Location() *time.Location
	NotifyDefault() cronpkg.NotifyTarget
	StartedAt() time.Time
}

// SchedulerForTest exposes the scheduler so the wiring side can assert that a
// disabled cron leaves a NIL interface rather than an interface wrapping a nil
// pointer (#2561; see TestCronHandlers_NilSchedulerStaysNilInterface).
func (h *Handlers) SchedulerForTest() SchedulerView { return h.deps.Scheduler }
