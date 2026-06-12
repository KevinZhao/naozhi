package cron

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/textutil"
)

// ErrInvalidPrompt is returned by ValidatePromptStrict when a prompt fails
// the shared cron-prompt safety policy (size cap / UTF-8 / C0 / DEL / C1 /
// bidi / LS / PS). Sentinel form so IM dispatch and Scheduler.SetJobPrompt
// can errors.Is and surface a stable user message instead of string-matching.
//
// R20260603140013-ARCH-1 (#1707): the validator + its sentinel moved to the
// leaf package internal/textutil so the IM dispatch edge can validate without
// importing this domain package. Aliased here (same value) so existing cron
// and dashboard callers' errors.Is checks keep matching.
var ErrInvalidPrompt = textutil.ErrInvalidCronPrompt

// ValidatePromptStrict enforces the shared cron-prompt size + character policy.
// Thin alias of textutil.ValidateCronPromptStrict; see that function for the
// policy. Kept so SetJobPrompt / dashboard callers stay unchanged.
// R20260603140013-ARCH-1 (#1707).
func ValidatePromptStrict(prompt string) error {
	return textutil.ValidateCronPromptStrict(prompt)
}

// ErrInvalidSchedule is returned by ValidateScheduleChars when a schedule
// expression fails the shared char policy. Aliased to the textutil sentinel
// (same value) so errors.Is checks keep matching. R20260603140013-ARCH-1 (#1707).
var ErrInvalidSchedule = textutil.ErrInvalidCronSchedule

// ValidateScheduleChars enforces the shared cron-schedule size + character
// policy. Thin alias of textutil.ValidateCronScheduleChars; see that function
// for the policy. R20260603140013-ARCH-1 (#1707).
func ValidateScheduleChars(schedule string) error {
	return textutil.ValidateCronScheduleChars(schedule)
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
// internal/server) cannot persist arbitrary Title / Prompt / WorkDir /
// Backend / Notify* bytes into cron_jobs.json.
//
// R20260607-ARCH-3 (#1927): Title (rune cap) and Prompt (ValidatePromptStrict)
// were previously enforced inline in AddJob, leaving validateJobFields an
// INCOMPLETE write-path gate — a future caller reaching it directly (its
// godoc invites exactly that: "kept exported so test fixtures and IM dispatch
// can share the same policy") would silently skip Title/Prompt validation.
// Folding both into this helper makes it the single complete write-path gate;
// AddJob now calls only validateJobFields. UpdateJob keeps its per-field delta
// path (it validates *upd.X partials, not a whole *Job).
//
// Empty values are allowed because the dashboard creates jobs with optional
// WorkDir / Backend / Notify* fields zero and a paused-with-empty-prompt
// state. The policy only rejects values that exceed the cap or smuggle
// log-injection / non-UTF-8 bytes.
//
// R250-CR-8 (#1141): defence-in-depth — IM dispatch (commands.go
// ParseCronAdd) and dashboard handlers (validateCron*) validate before
// calling AddJob, but a future internal caller (or test fixture) reaching
// AddJob directly would otherwise persist arbitrary bytes for these fields.
// Kept exported so test fixtures and IM dispatch can share the same policy.
func validateJobFields(j *Job) error {
	// R20260607-ARCH-3 (#1927): Title rune-cap, folded from AddJob. Title 长度
	// 校验在 scheduler 层兜底，避免绕过 dashboard handler（例如 store 直接加载
	// 被篡改的 cron_jobs.json）把超长字符串持久化进内存。
	if n := utf8.RuneCountInString(j.Title); n > MaxCronTitleLen {
		return fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}
	// R20260607-ARCH-3 (#1927): Prompt strict validation, folded from AddJob
	// (R244-SEC-P2-5 / #889). Empty prompts are permitted because the dashboard
	// creates jobs in a paused-with-empty-prompt state to be filled in via
	// SetJobPrompt later.
	if j.Prompt != "" {
		if err := ValidatePromptStrict(j.Prompt); err != nil {
			return err
		}
	}
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
	if err := validatePlacement(j.Placement); err != nil {
		return fmt.Errorf("cron: %w", err)
	}
	// Phase 1 sandbox guardrail (RFC §4.4): cross-field combination gate,
	// mirrored in UpdateJob's critical section for the patch path.
	if placementIsSandbox(j.Placement) && j.WorkDir != "" {
		return ErrSandboxWorkDir
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
	// MaxPromptBytes / MaxIDLen / MaxScheduleBytes are aliased from the leaf
	// package internal/textutil (R20260603140013-ARCH-1, #1707) so the IM
	// dispatch and dashboard edges can share the same bounds without importing
	// this domain package. Kept here so existing cron / dashboard call sites
	// stay unchanged.
	MaxPromptBytes   = textutil.MaxCronPromptBytes
	MaxIDLen         = textutil.MaxCronIDLen
	MaxScheduleBytes = textutil.MaxCronScheduleBytes

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
