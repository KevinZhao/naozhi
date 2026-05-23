package cron

import "github.com/naozhi/naozhi/internal/textutil"

// truncatedSuffix marks where truncateWithSuffix cut a string that exceeded
// the rune budget. Centralised so any downstream byte-cap can compensate for
// its byte length (see truncateWithSuffix call sites that pass
// maxStoredResultRunes+len(truncatedSuffix) into SanitizeForLog).
const truncatedSuffix = "…[truncated]"

// truncateWithSuffix returns s rune-truncated to maxRunes, appending
// truncatedSuffix only when the input was actually shrunk. R234-CR-1:
// previously sanitiseRunResult / recordResultP0WithSanitised / truncateForRetry
// each open-coded the same `if shrunk < s { s = trimmed + truncatedSuffix }`
// pattern, so adding a "…[shortened]" variant or changing the trim-on-equal
// rule required hunting three sites. Centralised here so future tweaks are
// one diff and grep-discoverable. Idempotent on already-clean strings.
func truncateWithSuffix(s string, maxRunes int) string {
	trimmed := textutil.TruncateRunesNoEllipsis(s, maxRunes)
	if len(trimmed) >= len(s) {
		return s
	}
	return trimmed + truncatedSuffix
}

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
	// recordResultP0WithSanitised) previously each
	// declared this as a function-local const, drifting in lockstep
	// only by convention.
	maxStoredResultRunes = 4 * 1024

	// maxCronErrMsgRunes bounds error strings persisted to cron_jobs.json
	// and broadcast to dashboard clients. SanitizeForLog runs after
	// redactPathsInCronError, so this cap is intentionally tighter than
	// the result cap — error classifiers ("permission denied", "context
	// deadline exceeded") fit comfortably within 512 runes and anything
	// longer is almost always carrying redacted-path context that the
	// dashboard truncates again on the wire. R230B-CR-5.
	maxCronErrMsgRunes = 512

	// maxRedactErrLen pre-truncates byte-length before redactPathsInCronError
	// runs its O(n) scan + Builder allocation. It is deliberately larger
	// than maxCronErrMsgRunes so a UTF-8-heavy errMsg whose rune count
	// equals the SanitizeForLog cap survives intact through redaction
	// (worst-case 4 bytes/rune). R230B-CR-5.
	maxRedactErrLen = 2048
)
