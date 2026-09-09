// consumer.go — the consumer-side dependency interface this package needs
// (#2561).
//
// Deps named *cron.Scheduler, whose 48 exported methods cover scheduling,
// persistence, the sandbox runner and the notify plumbing. SchedulerView is the
// MEASURED call surface — grep h.deps.Scheduler.<Method> across the package — at 23.
//
// 23 of 48 is a narrower cut than dashproject's 3 of 77, and that is honest
// rather than disappointing: this package IS the cron UI, so it reads jobs, runs,
// the inflight view, the sandbox attention queue and the snapshot blobs, and it
// writes every job mutation the dashboard offers. What the interface buys is that
// the other 25 — the tick loop, the store internals, the notify senders — can
// change without touching the handlers, and that a test can fake the surface
// instead of standing up a real Scheduler.
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

// SchedulerView is the *cron.Scheduler surface the cron dashboard uses.
type SchedulerView interface {
	// Job CRUD reachable from the cron panel.
	AddJob(j *cronpkg.Job) error
	GetJob(id string) (cronpkg.Job, bool)
	ListJobs(plat, chatID string) []cronpkg.Job
	ListAllJobsWithNextRun() []cronpkg.JobWithNextRun
	UpdateJob(id string, upd cronpkg.JobUpdate) (*cronpkg.Job, error)
	DeleteJobByID(id string) (*cronpkg.Job, error)
	PauseJobByID(id string) (*cronpkg.Job, error)
	ResumeJobByID(id string) (*cronpkg.Job, error)
	TriggerNow(id string) error

	// Run history + the live run.
	Run(jobID, runID string) (*cronpkg.CronRun, error)
	ListRuns(jobID string, limit int, before time.Time) []cronpkg.CronRunSummary
	RecentRuns(jobID string, n int) []cronpkg.CronRunSummary
	CurrentRun(jobID string) (cronpkg.RunInflightView, bool)

	// Sandbox runs (docs/rfc/agentcore-cloud-sandbox.md §7.4).
	ListSandboxAttention() []cronpkg.SandboxAttentionItem
	ConfirmSandboxRun(runID string) error
	ReplaySandboxRun(jobID, origRunID string) (string, error)
	SandboxRunEvents(jobID, runID string, maxLines int) ([][]byte, bool, error)
	SandboxRunSnapshotManifest(jobID, runID string) (*cronpkg.SandboxRunSnapshot, bool, error)
	SandboxRunSnapshotPrompt(blobHash string) (string, error)

	// Schedule preview + facts the payloads carry.
	PreviewScheduleN(schedule string, n int) ([]time.Time, error)
	Location() *time.Location
	NotifyDefault() cronpkg.NotifyTarget
	StartedAt() time.Time
}

// SchedulerForTest exposes the scheduler so the wiring side can assert that a
// disabled cron leaves a NIL interface rather than an interface wrapping a nil
// pointer (#2561; see TestCronHandlers_NilSchedulerStaysNilInterface).
func (h *Handlers) SchedulerForTest() SchedulerView { return h.deps.Scheduler }
