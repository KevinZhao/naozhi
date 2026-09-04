package cron

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/textutil"
)

// ErrInvalidPrompt is returned by ValidatePromptStrict when a prompt fails
// the shared cron-prompt safety policy (size cap / UTF-8 / C0 / DEL / C1 /
// bidi / LS / PS). Alias of the textutil sentinel (same value) so IM dispatch
// and dashboard callers' errors.Is checks keep matching (#1707).
var ErrInvalidPrompt = textutil.ErrInvalidCronPrompt

// ValidatePromptStrict enforces the shared cron-prompt size + character policy.
// Thin alias of textutil.ValidateCronPromptStrict; see that function for the policy.
func ValidatePromptStrict(prompt string) error {
	return textutil.ValidateCronPromptStrict(prompt)
}

// ErrInvalidSchedule is returned by ValidateScheduleChars when a schedule
// expression fails the shared char policy. Alias of the textutil sentinel.
var ErrInvalidSchedule = textutil.ErrInvalidCronSchedule

// ValidateScheduleChars enforces the shared cron-schedule size + character
// policy. Thin alias of textutil.ValidateCronScheduleChars.
func ValidateScheduleChars(schedule string) error {
	return textutil.ValidateCronScheduleChars(schedule)
}

// MaxWorkDirLen caps Job.WorkDir on the AddJob write path. 4 KiB matches the
// de-facto Linux PATH_MAX; longer values cannot reach a real filesystem.
// loadJobs applies the same cap on the read path.
const MaxWorkDirLen = 4096

// MaxBackendLen caps Job.Backend on the AddJob write path before any session
// code sees the bytes; 64 covers every backend ID with slack.
const MaxBackendLen = 64

// MaxNotifyTargetLen caps Job.NotifyPlatform / Job.NotifyChatID on the AddJob
// write path; both flow into the dashboard broadcast and webhook URLs.
const MaxNotifyTargetLen = 256

// validateJobFields is the single complete write-path gate for AddJob: it
// mirrors loadJobs's read-side validation so an internal caller bypassing the
// dashboard / IM validators cannot persist arbitrary Title / Prompt / WorkDir /
// Backend / Notify* bytes into cron_jobs.json (#1141, #1927). UpdateJob keeps
// its per-field delta path. Empty values are allowed (dashboard creates jobs
// with optional fields zero and a paused-with-empty-prompt state); only values
// over the cap or carrying log-injection / non-UTF-8 bytes are rejected.
func validateJobFields(j *Job) error {
	// Title 长度校验在 scheduler 层兜底，避免绕过 dashboard handler 把超长字符串持久化。
	if n := utf8.RuneCountInString(j.Title); n > MaxCronTitleLen {
		return fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}
	// Empty prompts are permitted: the dashboard creates paused jobs to be filled
	// in via SetJobPrompt.
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
// truncatedSuffix only when the input was actually shrunk. Idempotent on
// already-clean strings.
func truncateWithSuffix(s string, maxRunes int) string {
	trimmed := textutil.TruncateRunesNoEllipsis(s, maxRunes)
	if len(trimmed) >= len(s) {
		return s
	}
	return trimmed + truncatedSuffix
}

// Shared input bounds for cron-related trust boundaries (IM `/cron` commands
// and dashboard HTTP endpoints). Both surfaces guard the same on-disk
// cron_jobs.json schema, so the limits must stay in lockstep.
const (
	// Aliased from the leaf package internal/textutil so the IM dispatch and
	// dashboard edges share the bounds without importing cron (#1707).
	MaxPromptBytes   = textutil.MaxCronPromptBytes
	MaxIDLen         = textutil.MaxCronIDLen
	MaxScheduleBytes = textutil.MaxCronScheduleBytes

	// maxStoredResultRunes bounds CronRun.Result + Job.LastResult after rune-safe
	// truncation; the record is hard-capped at MaxRunRecordBytes downstream, but
	// trimming early avoids carrying multi-KB strings through SanitizeForLog.
	maxStoredResultRunes = 4 * 1024

	// maxCronErrMsgRunes bounds error strings persisted to cron_jobs.json and
	// broadcast to dashboards. Tighter than the result cap: error classifiers fit
	// in 512 runes and anything longer is mostly redacted-path context.
	maxCronErrMsgRunes = 512

	// maxRedactErrLen pre-truncates byte-length before redactPathsInCronError's
	// O(n) scan. Larger than maxCronErrMsgRunes so a UTF-8-heavy errMsg at the
	// rune cap survives redaction intact (worst-case 4 bytes/rune).
	maxRedactErrLen = 2048

	// redactFastPathMaxLen caps the input length for redactPathsInCronError's
	// zero-alloc fast path (no path-trigger byte → return the aliased input).
	// Fits common error classifiers while keeping a defensive ceiling (#1115).
	redactFastPathMaxLen = 256

	// previousTickMaxIter caps previousTickBefore's sched.Next loop; 1000 leaves
	// ~3× margin over the worst legitimate case (~365 iterations for a daily
	// schedule across DST/leap-month).
	previousTickMaxIter = 1000
)

// CronRun history limits, in two const blocks so a future SchedulerConfig
// knob change cannot accidentally relax a hard schema cap.

// User-configurable defaults — fallbacks when SchedulerConfig leaves
// RunsKeepCount / RunsKeepWindow zero.
const (
	// DefaultRunsKeepCount caps per-job history at this many entries.
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
