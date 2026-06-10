// cron_consumer.go declares the dispatch-side consumer surface for the
// /cron slash commands. R250-ARCH-1 (#1164): dispatch previously imported
// internal/cron for the CronScheduler seam (*cron.Job in every signature),
// pinning a dispatch→cron edge that RFC cron-sysession-merge §3.6 requires
// cut. The types here are dispatch-owned projections; the translation to
// concrete cron types lives in the host adapter
// (internal/server/cron_dispatch_adapter.go), mirroring the
// send_dispatch_adapter.go precedent.
package dispatch

import "time"

// CronJob is the dispatch-side projection of a cron job. It carries only
// the fields the /cron command handlers actually read (ID / Schedule /
// Prompt / Paused) — NOT the full cron.Job shape. Adding a field here
// means a handler started reading it; the adapter's projection function
// must be extended in the same change. #1164.
type CronJob struct {
	ID       string
	Schedule string
	Prompt   string
	Paused   bool
}

// CronJobRequest carries the creation parameters for /cron add. The host
// adapter translates it into the concrete job construction call
// (cron.NewJob + JobIMContext); dispatch never sees the cron types. #1164.
type CronJobRequest struct {
	Schedule  string
	Prompt    string
	Platform  string
	ChatID    string
	ChatType  string
	CreatedBy string
}

// Dispatch-side cron error codes. The string values MUST stay byte-identical
// to the wire values of the corresponding cron.ErrCode constants in
// internal/cron/error_class.go (CodeJobNotFound / CodeAmbiguousPrefix /
// CodeJobAlreadyPaused / CodeJobNotPaused / CodeInvalidPrompt) — the
// CronCommands.ClassifyError contract is defined in terms of those wire
// values. The correspondence is pinned by a server-side contract test
// (internal/server/cron_dispatch_adapter_test.go) so a drift on either side
// fails the build there, local to the adapter that owns the translation.
// #1164.
const (
	CronCodeJobNotFound      = "job_not_found"
	CronCodeAmbiguousPrefix  = "ambiguous_prefix"
	CronCodeJobAlreadyPaused = "job_already_paused"
	CronCodeJobNotPaused     = "job_not_paused"
	CronCodeInvalidPrompt    = "invalid_prompt"
)

// CronCommands is the consumer-side seam that dispatch's slash-command
// handlers (handleCronAdd / handleCronList / handleCronDel /
// handleCronPause / handleCronResume) require.
//
// R250-ARCH-17 (#1178) introduced the original CronScheduler seam so tests
// could stand up a fake without a real Scheduler + tempdir + persistence
// loop. R250-ARCH-1 (#1164) replaces it with this projection-typed
// interface so dispatch no longer imports internal/cron at all: production
// wiring passes the server-side cronDispatchAdapter
// (internal/server/cron_dispatch_adapter.go), which translates to/from the
// concrete *cron.Scheduler.
//
// AddJob and ResumeJob fold the previous separate NextRun call into their
// return value: both handlers were the only NextRun callers and invoked it
// immediately after the mutation on the same job, so the merge loses no
// semantics and removes *cron.Job (the NextRun parameter) from the seam.
type CronCommands interface {
	// AddJob creates and registers a job from req, returning the projection
	// and its next scheduled run time. On error the returned error MUST
	// preserve the scheduler's sentinel chain so ClassifyError still
	// resolves it (the adapter must not wrap with %v).
	AddJob(req CronJobRequest) (CronJob, time.Time, error)
	ListJobs(plat, chatID string) []CronJob
	DeleteJob(idPrefix, plat, chatID string) (CronJob, error)
	PauseJob(idPrefix, plat, chatID string) (CronJob, error)
	// ResumeJob resumes a paused job and returns its next scheduled run
	// time (see the AddJob/NextRun merge note above).
	ResumeJob(idPrefix, plat, chatID string) (CronJob, time.Time, error)
	// ClassifyError maps a scheduler-returned error to a stable wire code —
	// the string value of the matching cron.ErrCode (see the CronCode*
	// constants above). Implementations return the cron-side classifier's
	// result verbatim; dispatch compares against the constants only.
	ClassifyError(err error) string
}
