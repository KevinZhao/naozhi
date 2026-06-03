// Package cron — limits_alias.go: thin aliases over the IM + dashboard shared
// input validators and bounds now living in internal/textutil.
//
// History: ValidatePromptStrict / ValidateScheduleChars / EscapeMarkdownPunct
// and the MaxPromptBytes / MaxScheduleBytes / MaxIDLen bounds were first
// written here. internal/dispatch (the IM slash-command layer) and
// internal/dashboard/cron imported the cron domain package solely to reach
// them, coupling unrelated layers to cron. R20260603-ARCH (#1707) relocated
// the pure string/byte/rune policy to the leaf package internal/textutil —
// the same rationale that moved RedactSecrets there under #1571 — so each
// consumer imports the leaf directly. These aliases keep cron's in-package
// call sites and exported symbols stable; NewJob / ClassifyError and the
// on-disk schema remain in cron.

package cron

import "github.com/naozhi/naozhi/internal/textutil"

// Shared input bounds for cron trust boundaries. Aliased from textutil so the
// many in-package (scheduler_finish / store / scheduler_jobs) and dashboard
// call sites stay unchanged. See internal/textutil/cron_validators.go for the
// rationale on each cap.
const (
	MaxPromptBytes   = textutil.MaxPromptBytes
	MaxScheduleBytes = textutil.MaxScheduleBytes
	MaxIDLen         = textutil.MaxIDLen
)

// ErrInvalidPrompt / ErrInvalidSchedule are re-exported so existing
// errors.Is(err, cron.ErrInvalidPrompt) call sites (error_class.go, dispatch
// seam tests, cron title tests) keep working — the var aliases reference the
// same sentinel the textutil validators wrap.
var (
	ErrInvalidPrompt   = textutil.ErrInvalidPrompt
	ErrInvalidSchedule = textutil.ErrInvalidSchedule
)

// ValidatePromptStrict enforces the shared cron-prompt safety policy.
//
// Deprecated: use textutil.ValidatePromptStrict directly. Retained as a thin
// alias for cron's in-package call sites (scheduler_jobs.go) and exported
// compatibility (#1707).
func ValidatePromptStrict(prompt string) error { return textutil.ValidatePromptStrict(prompt) }

// ValidateScheduleChars enforces the shared cron-schedule char policy.
//
// Deprecated: use textutil.ValidateScheduleChars directly (#1707).
func ValidateScheduleChars(schedule string) error { return textutil.ValidateScheduleChars(schedule) }

// EscapeMarkdownPunct neutralises markdown link-syntax punctuation. Exported
// alias for packages displaying cron Job fields in IM replies (#1707, formerly
// R112714-ARCH-1).
//
// Deprecated: use textutil.EscapeMarkdownPunct directly.
func EscapeMarkdownPunct(s string) string { return textutil.EscapeMarkdownPunct(s) }
