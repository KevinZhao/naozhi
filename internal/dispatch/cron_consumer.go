// cron_consumer.go declares the dispatch-side consumer surface for the /cron
// slash commands. The types here are dispatch-owned projections so dispatch
// does not import internal/cron (RFC cron-sysession-merge §3.6); the
// translation to concrete cron types lives in the host adapter
// (internal/server/cron_dispatch_adapter.go) (#1164).
package dispatch

import "time"

// CronJob is the dispatch-side projection of a cron job: only the fields the
// /cron handlers read. Adding a field here requires extending the adapter's
// projection function in the same change.
type CronJob struct {
	ID       string
	Schedule string
	Prompt   string
	Paused   bool
}

// CronJobRequest carries the creation parameters for /cron add; the host
// adapter translates it into the concrete job construction call.
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
// internal/cron/error_class.go — CronCommands.ClassifyError is defined in
// terms of those wire values. Pinned by a contract test in
// internal/server/cron_dispatch_adapter_test.go.
const (
	CronCodeJobNotFound      = "job_not_found"
	CronCodeAmbiguousPrefix  = "ambiguous_prefix"
	CronCodeJobAlreadyPaused = "job_already_paused"
	CronCodeJobNotPaused     = "job_not_paused"
	CronCodeInvalidPrompt    = "invalid_prompt"
)

// CronCommands is the consumer-side seam dispatch's /cron slash-command
// handlers require. Production wiring passes the server-side
// cronDispatchAdapter, which translates to/from *cron.Scheduler; tests use a
// fake. AddJob and ResumeJob return the next run time directly so *cron.Job
// never appears in the seam.
type CronCommands interface {
	// AddJob creates and registers a job from req, returning the projection
	// and its next scheduled run time. On error the returned error MUST
	// preserve the scheduler's sentinel chain so ClassifyError still
	// resolves it (the adapter must not wrap with %v).
	AddJob(req CronJobRequest) (CronJob, time.Time, error)
	ListJobs(plat, chatID string) []CronJob
	DeleteJob(idPrefix, plat, chatID string) (CronJob, error)
	PauseJob(idPrefix, plat, chatID string) (CronJob, error)
	// ResumeJob resumes a paused job and returns its next scheduled run time.
	ResumeJob(idPrefix, plat, chatID string) (CronJob, time.Time, error)
	// ClassifyError maps a scheduler-returned error to a stable wire code (one
	// of the CronCode* constants), returned verbatim from the cron-side classifier.
	ClassifyError(err error) string
}
