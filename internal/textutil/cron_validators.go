// Package textutil — cron_validators.go: dependency-free input validators and
// markdown-punct escaping shared by the IM `/cron` slash-command edge
// (internal/dispatch), the dashboard cron HTTP edge (internal/dashboard/cron),
// and the cron scheduler itself (internal/cron).
//
// R20260603140013-ARCH-1 (#1707): these helpers began life in internal/cron
// (ValidatePromptStrict / ValidateScheduleChars / MaxIDLen / EscapeMarkdownPunct).
// IM dispatch imported them by concrete type, coupling the slash-command layer
// to the cron domain package so dispatch was recompiled on any cron change and
// could not be tested without the real cron package. The logic carries zero
// cron semantics — it is pure input-character / size / markdown policy — so it
// now lives in this leaf package (same rationale that moved RedactSecrets here,
// #1571). internal/cron keeps thin aliases so its own callers and the dashboard
// edge stay unchanged; dispatch imports textutil directly.
//
// Policies must stay in lockstep across all surfaces because they all guard the
// same on-disk cron_jobs.json schema.

package textutil

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Shared input bounds for cron-related trust boundaries (IM `/cron` commands
// and dashboard HTTP endpoints).
const (
	// MaxCronPromptBytes bounds the prompt body accepted by both the IM
	// `/cron add` command and the dashboard cron POST/PATCH endpoints. Every
	// cron run replays the full prompt through the CLI, so runaway sizes
	// multiply across invocations.
	MaxCronPromptBytes = 8 * 1024

	// MaxCronIDLen bounds cron job IDs flowing in via the IM `/cron <op> <id>`
	// commands and the dashboard URL/JSON parameters. Generated IDs are
	// 16-char hex; 64 bytes leaves slack for future ID schemes while
	// preventing multi-MB inputs from propagating into log/error allocations
	// on the miss path.
	MaxCronIDLen = 64

	// MaxCronScheduleBytes caps the schedule expression length. robfig/cron
	// expressions are short (e.g. "@every 30m", "0 9 * * *"); anything beyond
	// this is almost certainly abuse.
	MaxCronScheduleBytes = 256
)

// ErrInvalidCronPrompt is returned by ValidateCronPromptStrict when a prompt
// fails the shared cron-prompt safety policy (size cap / UTF-8 / C0 / DEL /
// C1 / bidi / LS / PS). Sentinel form so callers can errors.Is and surface a
// stable user message instead of string-matching.
var ErrInvalidCronPrompt = errors.New("cron: invalid prompt")

// ErrInvalidCronSchedule is returned by ValidateCronScheduleChars when a
// schedule expression fails the shared char policy. Sentinel form so callers
// can errors.Is.
var ErrInvalidCronSchedule = errors.New("cron: invalid schedule")

// ValidateCronPromptStrict enforces the shared size + character policy for a
// cron prompt body before it is persisted to cron_jobs.json.
//
// Policy:
//   - len ≤ MaxCronPromptBytes
//   - utf8.ValidString
//   - no C0 controls except \t \n \r; no DEL (0x7f)
//   - no rune flagged by osutil.IsLogInjectionRune (C1 / bidi / LS / PS)
//
// Returns a wrapped ErrInvalidCronPrompt. Empty prompt is rejected here.
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
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return fmt.Errorf("%w: contains control characters", ErrInvalidCronPrompt)
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: contains unicode control characters", ErrInvalidCronPrompt)
		}
	}
	return nil
}

// ValidateCronScheduleChars enforces the shared character + size policy for a
// cron schedule expression before it reaches robfig/cron's parser.
//
// Policy:
//   - len ≤ MaxCronScheduleBytes
//   - utf8.ValidString
//   - no C0 controls and no DEL (0x7f); unlike prompts, schedules forbid
//     tab/newline too — robfig/cron expressions are whitespace-separated
//     single-line tokens, so an embedded \t or \n is always malformed
//   - no rune flagged by osutil.IsLogInjectionRune (C1 / bidi / LS / PS)
//
// Returns a wrapped ErrInvalidCronSchedule. Empty schedule is rejected here.
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

// cronMarkdownPunctReplacer replaces the markdown link-syntax characters
// `[`, `]`, `(`, `)` with full-width visually-similar codepoints
// (U+FF3B / U+FF3D / U+FF08 / U+FF09).
var cronMarkdownPunctReplacer = strings.NewReplacer(
	"[", "［",
	"]", "］",
	"(", "（",
	")", "）",
)

// EscapeCronMarkdownPunct replaces the markdown link-syntax characters
// `[`, `]`, `(`, `)` with full-width visually-similar codepoints so an
// attacker-controlled cron Title or result body cannot smuggle `[text](url)`
// clickable links into an IM notice. A ContainsAny fast-path avoids any
// allocation on the common ASCII-clean case. Idempotent on clean input.
func EscapeCronMarkdownPunct(s string) string {
	if !strings.ContainsAny(s, "[]()") {
		return s
	}
	return cronMarkdownPunctReplacer.Replace(s)
}
