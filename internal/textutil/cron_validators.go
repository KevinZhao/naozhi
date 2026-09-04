// cron_validators.go: dependency-free input validators and markdown-punct
// escaping shared by the IM `/cron` edge, the dashboard cron HTTP edge and
// the cron scheduler. Policies must stay in lockstep across all surfaces
// because they all guard the same on-disk cron_jobs.json schema (#1707).

package textutil

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Shared input bounds for the cron trust boundaries (IM `/cron`, dashboard HTTP).
const (
	// MaxCronPromptBytes bounds the prompt body accepted by `/cron add` and the
	// dashboard cron endpoints; every run replays the full prompt via the CLI.
	MaxCronPromptBytes = 8 * 1024

	// MaxCronIDLen bounds cron job IDs from IM commands and dashboard URL/JSON
	// parameters. Generated IDs are 16-char hex; 64 leaves slack for future schemes.
	MaxCronIDLen = 64

	// MaxCronScheduleBytes caps the schedule expression; robfig/cron expressions
	// are short ("@every 30m", "0 9 * * *"), so anything beyond this is abuse.
	MaxCronScheduleBytes = 256
)

// ErrInvalidCronPrompt is returned (wrapped) by ValidateCronPromptStrict so
// callers can errors.Is and surface a stable user message.
var ErrInvalidCronPrompt = errors.New("cron: invalid prompt")

// ErrInvalidCronSchedule is returned (wrapped) by ValidateCronScheduleChars.
var ErrInvalidCronSchedule = errors.New("cron: invalid schedule")

// ValidateCronPromptStrict enforces the shared size + character policy for a
// cron prompt body before it is persisted to cron_jobs.json: len ≤
// MaxCronPromptBytes, valid UTF-8, no C0 controls except \t \n \r, no DEL,
// and no rune flagged by osutil.IsLogInjectionRune (C1 / bidi / LS / PS).
// Returns a wrapped ErrInvalidCronPrompt; empty prompt is rejected.
func ValidateCronPromptStrict(prompt string) error {
	if prompt == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidCronPrompt)
	}
	if len(prompt) > MaxCronPromptBytes {
		return fmt.Errorf("%w: exceeds %d-byte limit", ErrInvalidCronPrompt, MaxCronPromptBytes)
	}
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("%w: contains invalid UTF-8", ErrInvalidCronPrompt)
	}
	anyHighBit := false
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c >= 0x80 {
			anyHighBit = true
			continue
		}
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return fmt.Errorf("%w: contains control characters", ErrInvalidCronPrompt)
	}
	// IsLogInjectionRune only flags non-ASCII runes, so a pure-ASCII prompt
	// skips the rune-decoding scan.
	if !anyHighBit {
		return nil
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: contains unicode control characters", ErrInvalidCronPrompt)
		}
	}
	return nil
}

// ValidateCronScheduleChars enforces the shared character + size policy for a
// cron schedule expression before it reaches robfig/cron's parser: len ≤
// MaxCronScheduleBytes, valid UTF-8, no C0 controls or DEL (unlike prompts,
// tab/newline are forbidden too — expressions are single-line tokens), and
// no rune flagged by osutil.IsLogInjectionRune. Returns a wrapped
// ErrInvalidCronSchedule; empty schedule is rejected.
func ValidateCronScheduleChars(schedule string) error {
	if len(schedule) == 0 {
		return fmt.Errorf("%w: must not be empty", ErrInvalidCronSchedule)
	}
	if len(schedule) > MaxCronScheduleBytes {
		return fmt.Errorf("%w: exceeds %d-byte limit", ErrInvalidCronSchedule, MaxCronScheduleBytes)
	}
	if !utf8.ValidString(schedule) {
		return fmt.Errorf("%w: contains invalid UTF-8", ErrInvalidCronSchedule)
	}
	anyHighBit := false
	for i := 0; i < len(schedule); i++ {
		c := schedule[i]
		if c >= 0x80 {
			anyHighBit = true
			continue
		}
		if c >= 0x20 && c != 0x7f {
			continue
		}
		return fmt.Errorf("%w: contains control characters", ErrInvalidCronSchedule)
	}
	if !anyHighBit {
		return nil
	}
	for _, r := range schedule {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: contains unicode control characters", ErrInvalidCronSchedule)
		}
	}
	return nil
}

// cronMarkdownPunctReplacer maps `[` `]` `(` `)` to full-width look-alikes.
var cronMarkdownPunctReplacer = strings.NewReplacer(
	"[", "［",
	"]", "］",
	"(", "（",
	")", "）",
)

// EscapeCronMarkdownPunct replaces the markdown link-syntax characters
// `[`, `]`, `(`, `)` with full-width look-alikes so an attacker-controlled
// cron Title or result body cannot smuggle `[text](url)` clickable links into
// an IM notice. Allocation-free on the common clean case; idempotent.
func EscapeCronMarkdownPunct(s string) string {
	if !strings.ContainsAny(s, "[]()") {
		return s
	}
	return cronMarkdownPunctReplacer.Replace(s)
}
