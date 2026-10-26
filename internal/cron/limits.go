package cron

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cron/runstore"
	"github.com/naozhi/naozhi/internal/textutil"
)

// ErrInvalidPrompt wraps every prompt rejection from ValidatePromptStrict.
var ErrInvalidPrompt = errors.New("cron: invalid prompt")

// ErrInvalidSchedule wraps every schedule rejection from AddJob / UpdateJob.
var ErrInvalidSchedule = errors.New("cron: invalid schedule")

// ValidatePromptStrict is the one cron-prompt policy, enforced on the scheduler
// write paths (AddJob / UpdateJob / SetJobPrompt): non-empty, at most
// MaxPromptBytes, valid UTF-8, and no byte containsCronUnsafe rejects.
func ValidatePromptStrict(prompt string) error {
	switch {
	case prompt == "":
		return fmt.Errorf("%w: must not be empty", ErrInvalidPrompt)
	case len(prompt) > MaxPromptBytes:
		return fmt.Errorf("%w: exceeds %d-byte limit", ErrInvalidPrompt, MaxPromptBytes)
	case !utf8.ValidString(prompt) || containsCronUnsafe(prompt):
		return fmt.Errorf("%w: contains invalid characters", ErrInvalidPrompt)
	}
	return nil
}

// checkJobField applies the shared length + UTF-8 + containsCronUnsafe guard
// to one free-text Job field.
func checkJobField(name, v string, maxLen int) error {
	if len(v) > maxLen {
		return fmt.Errorf("cron: %s too long: %d bytes > %d cap", name, len(v), maxLen)
	}
	if !utf8.ValidString(v) || containsCronUnsafe(v) {
		return fmt.Errorf("cron: %s contains invalid bytes", name)
	}
	return nil
}

// validateTitle gates Job.Title on the write paths: at most MaxCronTitleLen
// runes, single line, and the same UTF-8 + containsCronUnsafe policy loadJobs
// applies on read, so a title accepted here is never dropped at the next boot.
// Empty is allowed (the UI falls back to the prompt's first line).
func validateTitle(title string) error {
	if n := utf8.RuneCountInString(title); n > MaxCronTitleLen {
		return fmt.Errorf("cron: title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}
	if !utf8.ValidString(title) || strings.ContainsAny(title, "\n\r") || containsCronUnsafe(title) {
		return fmt.Errorf("cron: title contains invalid characters")
	}
	return nil
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

// validateJobFields is the one write-path gate for AddJob (the dashboard and
// IM edges do not re-check these fields): it mirrors loadJobs's read-side
// validation so no caller can persist arbitrary Title / Prompt / WorkDir /
// Backend / Notify* bytes into cron_jobs.json (#1141, #1927). UpdateJob checks
// the same rules per patched field. Empty values are allowed (dashboard creates
// paused-with-empty-prompt jobs); only values over the cap or carrying
// log-injection / non-UTF-8 bytes are rejected.
func validateJobFields(j *Job) error {
	if err := validateTitle(j.Title); err != nil {
		return err
	}
	// Empty prompts are permitted: the dashboard creates paused jobs to be filled
	// in via SetJobPrompt.
	if j.Prompt != "" {
		if err := ValidatePromptStrict(j.Prompt); err != nil {
			return err
		}
	}
	if err := checkJobField("work_dir", j.WorkDir, MaxWorkDirLen); err != nil {
		return err
	}
	if err := checkJobField("backend", j.Backend, MaxBackendLen); err != nil {
		return err
	}
	if err := checkJobField("notify_platform", j.NotifyPlatform, MaxNotifyTargetLen); err != nil {
		return err
	}
	if err := checkJobField("notify_chat_id", j.NotifyChatID, MaxNotifyTargetLen); err != nil {
		return err
	}
	return nil
}

// truncatedSuffix / truncateWithSuffix are the run store's truncation marker
// and helper; cron_jobs.json fields use the same marker as run records.
const truncatedSuffix = runstore.TruncatedSuffix

func truncateWithSuffix(s string, maxRunes int) string {
	return runstore.TruncateWithSuffix(s, maxRunes)
}

// Input bounds for cron_jobs.json fields.
const (
	// MaxPromptBytes bounds a job prompt; every run replays it via the CLI.
	MaxPromptBytes = textutil.MaxCronPromptBytes
	// MaxIDLen bounds job IDs from IM commands and dashboard parameters.
	// Generated IDs are 16-char hex; 64 leaves slack.
	MaxIDLen = 64
	// MaxScheduleBytes caps a schedule expression ("@every 30m", "0 9 * * *").
	MaxScheduleBytes = 256

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

	// previousTickMaxIter caps previousTickBeforeFromSched's sched.Next loop; 1000 leaves
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
