package cron

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// ErrInvalidPrompt is returned by ValidatePromptStrict when a prompt fails
// the shared cron-prompt safety policy (size cap / UTF-8 / C0 / DEL / C1 /
// bidi / LS / PS). Sentinel form so IM dispatch and Scheduler.SetJobPrompt
// can errors.Is and surface a stable user message instead of string-matching.
var ErrInvalidPrompt = errors.New("cron: invalid prompt")

// ValidatePromptStrict enforces the same size + character policy that
// dashboard's validateCronPrompt applies on the HTTP edge, so the IM
// `/cron …` path (Hub.runTurn / runTurnPassthrough → SetJobPrompt) cannot
// smuggle log-injection / bidi / oversized prompts onto cron_jobs.json by
// going around the dashboard validators. R243-SEC-8 (REPEAT-5):
// previously SetJobPrompt only rejected the empty string, so an IM-side
// caller could persist arbitrary bytes that the dashboard rejects.
//
// Policy (must stay in lockstep with server.validateCronPrompt):
//   - len ≤ MaxPromptBytes
//   - utf8.ValidString
//   - no C0 controls except \t \n \r; no DEL (0x7f)
//   - no rune flagged by osutil.IsLogInjectionRune (C1 / bidi / LS / PS)
//
// Returns a wrapped ErrInvalidPrompt so callers can distinguish from
// not-found / persist-failure errors. Empty prompt is rejected here as
// well so SetJobPrompt's pre-existing "must not be empty" guard becomes
// a single ValidatePromptStrict call.
func ValidatePromptStrict(prompt string) error {
	if prompt == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidPrompt)
	}
	if len(prompt) > MaxPromptBytes {
		return fmt.Errorf("%w: exceeds %d-byte limit", ErrInvalidPrompt, MaxPromptBytes)
	}
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("%w: contains invalid UTF-8", ErrInvalidPrompt)
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return fmt.Errorf("%w: contains control characters", ErrInvalidPrompt)
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: contains unicode control characters", ErrInvalidPrompt)
		}
	}
	return nil
}

// MaxWorkDirLen caps Job.WorkDir on the AddJob write path. 4 KiB matches the
// de-facto Linux PATH_MAX × small slack; longer values cannot legitimately
// reach a real filesystem and would just inflate every "could not chdir"
// slog line. Mirrors the same cap loadJobs (store.go ~L232) already applies
// on the read path. R250-CR-8 (#1141).
const MaxWorkDirLen = 4096

// MaxBackendLen caps Job.Backend on the AddJob write path. The session-side
// validateBackend already gates shape-invalid input, but cron's defence-in-
// depth bound also caps the bytes that flow into Job.Backend before any
// session code sees them. 64 covers every backend ID we ship today
// ("claude" / "kiro" / future short tags) with comfortable slack.
// R250-CR-8 (#1141).
const MaxBackendLen = 64

// MaxNotifyTargetLen caps Job.NotifyPlatform / Job.NotifyChatID on the
// AddJob write path. Both fields flow into the cronJobView dashboard
// broadcast and into platform-side webhook URLs; a hand-crafted internal
// caller bypassing the dashboard validators could otherwise smuggle a
// multi-MB string here. R250-CR-8 (#1141).
const MaxNotifyTargetLen = 256

// validateJobFields enforces the AddJob defence-in-depth policy that mirrors
// loadJobs's read-side validation (store.go) so an internal caller bypassing
// the dashboard validators (commands.go ParseCronAdd / validateCron* in
// internal/server) cannot persist arbitrary Prompt / WorkDir / Backend /
// Notify* bytes into cron_jobs.json. Title and Prompt are already enforced
// inline in AddJob; this helper covers the remaining fields the dashboard
// validates but the scheduler-level entry previously took on trust.
//
// Empty values are allowed because the dashboard creates jobs with optional
// WorkDir / Backend / Notify* fields zero. The policy only rejects values
// that exceed the cap or smuggle log-injection / non-UTF-8 bytes.
//
// R250-CR-8 (#1141): defence-in-depth — IM dispatch (commands.go
// ParseCronAdd) and dashboard handlers (validateCron*) validate before
// calling AddJob, but a future internal caller (or test fixture) reaching
// AddJob directly would otherwise persist arbitrary bytes for these fields.
// Kept exported so test fixtures and IM dispatch can share the same policy.
func validateJobFields(j *Job) error {
	if len(j.WorkDir) > MaxWorkDirLen {
		return fmt.Errorf("cron: work_dir too long: %d bytes > %d cap", len(j.WorkDir), MaxWorkDirLen)
	}
	if !utf8.ValidString(j.WorkDir) || containsCronUnsafe(j.WorkDir) {
		return fmt.Errorf("cron: work_dir contains invalid bytes")
	}
	if len(j.Backend) > MaxBackendLen {
		return fmt.Errorf("cron: backend too long: %d bytes > %d cap", len(j.Backend), MaxBackendLen)
	}
	if !utf8.ValidString(j.Backend) || containsCronUnsafe(j.Backend) {
		return fmt.Errorf("cron: backend contains invalid bytes")
	}
	if len(j.NotifyPlatform) > MaxNotifyTargetLen {
		return fmt.Errorf("cron: notify_platform too long: %d bytes > %d cap", len(j.NotifyPlatform), MaxNotifyTargetLen)
	}
	if !utf8.ValidString(j.NotifyPlatform) || containsCronUnsafe(j.NotifyPlatform) {
		return fmt.Errorf("cron: notify_platform contains invalid bytes")
	}
	if len(j.NotifyChatID) > MaxNotifyTargetLen {
		return fmt.Errorf("cron: notify_chat_id too long: %d bytes > %d cap", len(j.NotifyChatID), MaxNotifyTargetLen)
	}
	if !utf8.ValidString(j.NotifyChatID) || containsCronUnsafe(j.NotifyChatID) {
		return fmt.Errorf("cron: notify_chat_id contains invalid bytes")
	}
	return nil
}

// truncatedSuffix marks where truncateWithSuffix cut a string that exceeded
// the rune budget. Centralised so any downstream byte-cap can compensate for
// its byte length (see truncateWithSuffix call sites that pass
// maxStoredResultRunes+len(truncatedSuffix) into SanitizeForLog).
const truncatedSuffix = "…[truncated]"

// truncateWithSuffix returns s rune-truncated to maxRunes, appending
// truncatedSuffix only when the input was actually shrunk. R234-CR-1:
// previously sanitiseRunResult / recordResultP0WithSanitised
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
	// 16-char hex (8 entropy bytes → hex.EncodeToString; see generateHexID
	// / hexIDEntropyBytes in job.go); 64 bytes leaves slack for future ID
	// schemes while preventing multi-MB inputs from propagating into
	// log/error allocations on the miss path.
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

	// redactFastPathMaxLen caps the input length for the zero-alloc
	// fast-path in redactPathsInCronError: if the input is at or below
	// this length AND contains no path-trigger byte, the function returns
	// the aliased input without touching the truncate branch or the
	// Builder pool. Sized to comfortably fit common cron error
	// classifiers ("context deadline exceeded", "dispatcher queue full",
	// "session not found") while keeping a defensive ceiling so an
	// unexpectedly long no-path input still flows through the byte-cap
	// branch. R250-PERF-12 / #1115.
	redactFastPathMaxLen = 256

	// previousTickMaxIter caps previousTickBefore's sched.Next loop. See
	// the comment on previousTickBefore for the per-schedule-class
	// derivation; 1000 leaves a ~3× safety margin over the worst legitimate
	// case (~365 iterations for a daily schedule across DST/leap-month).
	// R235-CR-10.
	previousTickMaxIter = 1000
)

// CronRun history limits — collected here so a reader hunting for "how
// big can a run record be / how many do we keep" finds a single spot
// rather than chasing magic numbers across runstore.go. The constants
// fall into two policy classes that we keep in separate const blocks
// so a future SchedulerConfig knob change cannot accidentally relax a
// hard schema cap. R247-CR-12 / R247-CR-20 (#598).

// User-configurable defaults — fallbacks when SchedulerConfig leaves
// RunsKeepCount / RunsKeepWindow zero. Operators may raise / lower
// them via SchedulerConfig at construction time; cron-run-history.md
// §4.3 explains the 200 / 30d sizing rationale.
const (
	// DefaultRunsKeepCount caps per-job history at this many entries.
	// 200 is the user-confirmed upper bound (cron-run-history.md §4.3 +
	// chat conversation 2026-05-17).
	DefaultRunsKeepCount = 200

	// DefaultRunsKeepWindow ages out runs older than this even when the
	// per-job count is below the cap. AND-with-OR semantics: a run is
	// kept only when (count_rank ≤ keepCount) AND (age ≤ keepWindow);
	// either condition false → trim.
	DefaultRunsKeepWindow = 30 * 24 * time.Hour
)

// Hard limits — immutable per-record format invariants, not
// operator-tunable. Changing them requires a schema bump because old
// run.json files may exist on disk above the new cap.
const (
	// MaxRunRecordBytes caps a single CronRun JSON payload. The 4K rune
	// cap on Result + 512-rune cap on ErrorMsg + 8K Prompt + ~512
	// metadata add up to ~13 KiB worst case; 32 KiB leaves headroom.
	// Reading a file larger than this returns ErrCorruptRun.
	MaxRunRecordBytes = 32 * 1024
)
