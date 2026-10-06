package cron

import (
	"fmt"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cron/runstore"
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
// expression fails the shared char policy, and wraps every schedule rejection
// from AddJob / UpdateJob. Alias of the textutil sentinel.
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

// truncatedSuffix / truncateWithSuffix are the run store's truncation marker
// and helper; cron_jobs.json fields use the same marker as run records.
const truncatedSuffix = runstore.TruncatedSuffix

func truncateWithSuffix(s string, maxRunes int) string {
	return runstore.TruncateWithSuffix(s, maxRunes)
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

// CronRun history limits, re-exported from the run store that enforces them.
// The defaults are fallbacks when SchedulerConfig leaves RunsKeepCount /
// RunsKeepWindow zero; MaxRunRecordBytes is a per-record format invariant.
const (
	DefaultRunsKeepCount  = runstore.DefaultKeepCount
	DefaultRunsKeepWindow = runstore.DefaultKeepWindow
	MaxRunRecordBytes     = runstore.MaxRecordBytes
)
