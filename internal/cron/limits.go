package cron

// Shared input bounds for cron-related trust boundaries (IM `/cron` commands
// and dashboard HTTP endpoints). Centralising these here avoids the prior
// drift hazard where two duplicate constants (dispatch.maxCron* /
// server.maxCron*Dashboard) could silently diverge if one side tightened
// without the other. Both surfaces guard the same on-disk cron_jobs.json
// schema, so the limits must stay in lockstep. R216-CR-1.
const (
	// MaxPromptBytes bounds the prompt body accepted by both the
	// IM `/cron add` command and the dashboard cron POST/PATCH endpoints.
	// Every cron run replays the full prompt through the CLI, so runaway
	// sizes multiply across invocations.
	MaxPromptBytes = 8 * 1024

	// MaxIDLen bounds cron job IDs flowing in via the IM `/cron <op> <id>`
	// commands and the dashboard URL/JSON parameters. Generated IDs are
	// 8-char hex (see scheduler.generateID); 64 bytes leaves slack for
	// future ID schemes while preventing multi-MB inputs from propagating
	// into log/error allocations on the miss path.
	MaxIDLen = 64

	// MaxScheduleBytes caps the schedule expression length. robfig/cron
	// expressions are short (e.g. "@every 30m", "0 9 * * *"); anything
	// beyond this is almost certainly abuse.
	MaxScheduleBytes = 256

	// maxStoredResultRunes bounds CronRun.Result + Job.LastResult after
	// rune-safe truncation; the persisted record is hard-capped at
	// MaxRunRecordBytes (32 KB) downstream, but trimming early avoids
	// the cost of carrying multi-KB strings through SanitizeForLog and
	// JSON marshal. Three call sites (sanitiseRunResult /
	// recordResultP0WithSanitised / recordResult) previously each
	// declared this as a function-local const, drifting in lockstep
	// only by convention.
	maxStoredResultRunes = 4 * 1024

	// maxRunErrorRunes bounds the cron error-message component (errMsg) after
	// path redaction and SanitizeForLog. Three call sites (sanitiseRunErrMsg,
	// recordResultP0WithSanitised, recordResult) previously each repeated
	// `osutil.SanitizeForLog(s, 512)`; centralising avoids the same
	// drift hazard the maxStoredResultRunes consolidation already addressed.
	// R230B-CR-5.
	maxRunErrorRunes = 512

	// redactErrInputCap caps the *input* size redactPathsInCronError accepts
	// before truncating. Sized larger than maxRunErrorRunes because the
	// redactor runs before SanitizeForLog; it has to leave headroom for
	// the path-stripping rewrite to fit a budget-friendly final string.
	// R230B-CR-5.
	redactErrInputCap = 2048
)
