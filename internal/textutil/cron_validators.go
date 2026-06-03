// Package textutil — cron_validators.go: the IM + dashboard shared input
// validators for the `/cron …` slash-command and the dashboard cron HTTP
// edge. None of these carry cron domain semantics — they are pure
// string/byte/rune policy checks plus the markdown-punct escaper — so they
// live in this leaf package rather than internal/cron.
//
// R20260603-ARCH (#1707): internal/dispatch/commands.go previously imported
// internal/cron purely to reach ValidatePromptStrict / ValidateScheduleChars /
// MaxIDLen / EscapeMarkdownPunct, coupling the IM slash-command layer to the
// cron domain package. The scan logic has zero cron semantics (same rationale
// that moved RedactSecrets here under #1571), so it now lives here; cron keeps
// thin aliases (see limits_alias.go) and dispatch / dashboard import textutil
// directly. NewJob / ClassifyError and the on-disk schema stay behind the
// CronScheduler interface in internal/cron.
//
// Policy must stay in lockstep across the IM edge (dispatch.ParseCronAdd) and
// the dashboard HTTP edge (validateCronPrompt / validateCronScheduleChars) so
// the two surfaces cannot silently drift apart when a new forbidden character
// or cap is added on one side only.

package textutil

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Shared input bounds for cron-related trust boundaries (IM `/cron` commands
// and dashboard HTTP endpoints). Centralised so the two surfaces guard the
// same on-disk cron_jobs.json schema without drift. cron re-exports these as
// thin aliases (limits_alias.go) so existing cron/dashboard call sites stay
// unchanged.
const (
	// MaxPromptBytes bounds the prompt body accepted by both the IM
	// `/cron add` command and the dashboard cron POST/PATCH endpoints. Every
	// cron run replays the full prompt through the CLI, so runaway sizes
	// multiply across invocations.
	MaxPromptBytes = 8 * 1024

	// MaxScheduleBytes caps the schedule expression length. robfig/cron
	// expressions are short (e.g. "@every 30m", "0 9 * * *"); anything beyond
	// this is almost certainly abuse.
	MaxScheduleBytes = 256

	// MaxIDLen bounds cron job IDs flowing in via the IM `/cron <op> <id>`
	// commands and the dashboard URL/JSON parameters. Generated IDs are
	// 16-char hex; 64 bytes leaves slack for future ID schemes while
	// preventing multi-MB inputs from propagating into log/error allocations
	// on the miss path.
	MaxIDLen = 64
)

// ErrInvalidPrompt is returned by ValidatePromptStrict when a prompt fails the
// shared cron-prompt safety policy (size cap / UTF-8 / C0 / DEL / C1 / bidi /
// LS / PS). Sentinel form so IM dispatch and the dashboard can errors.Is and
// surface a stable user message instead of string-matching.
var ErrInvalidPrompt = errors.New("cron: invalid prompt")

// ErrInvalidSchedule is returned by ValidateScheduleChars when a schedule
// expression fails the shared char policy. Sentinel form so callers can
// errors.Is and surface a stable message.
var ErrInvalidSchedule = errors.New("cron: invalid schedule")

// ValidatePromptStrict enforces the size + character policy shared by the IM
// `/cron …` path and the dashboard cron edge so neither surface can smuggle
// log-injection / bidi / oversized prompts onto cron_jobs.json.
//
// Policy:
//   - non-empty
//   - len ≤ MaxPromptBytes
//   - utf8.ValidString
//   - no C0 controls except \t \n \r; no DEL (0x7f)
//   - no rune flagged by osutil.IsLogInjectionRune (C1 / bidi / LS / PS)
//
// Returns a wrapped ErrInvalidPrompt so callers can distinguish from
// not-found / persist-failure errors.
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

// ValidateScheduleChars enforces the shared character + size policy for a cron
// schedule expression before it reaches robfig/cron's parser.
//
// Policy:
//   - non-empty
//   - len ≤ MaxScheduleBytes
//   - utf8.ValidString
//   - no C0 controls and no DEL (0x7f); unlike prompts, schedules forbid
//     tab/newline too — robfig/cron expressions are whitespace-separated
//     single-line tokens, so an embedded \t or \n is always malformed
//   - no rune flagged by osutil.IsLogInjectionRune (C1 / bidi / LS / PS)
//
// Returns a wrapped ErrInvalidSchedule so callers can distinguish from
// parse-failure errors.
func ValidateScheduleChars(schedule string) error {
	if len(schedule) == 0 {
		return fmt.Errorf("%w: must not be empty", ErrInvalidSchedule)
	}
	if len(schedule) > MaxScheduleBytes {
		return fmt.Errorf("%w: exceeds %d-byte limit", ErrInvalidSchedule, MaxScheduleBytes)
	}
	if !utf8.ValidString(schedule) {
		return fmt.Errorf("%w: contains invalid UTF-8", ErrInvalidSchedule)
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
		return fmt.Errorf("%w: contains control characters", ErrInvalidSchedule)
	}
	if !anyHighBit {
		return nil
	}
	for _, r := range schedule {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: contains unicode control characters", ErrInvalidSchedule)
		}
	}
	return nil
}

// cronMarkdownPunctReplacer is a package-level Replacer for EscapeMarkdownPunct.
// Constructed once to avoid per-call allocation; Replace performs a single pass
// over the input string. R164930-PERF-4.
var cronMarkdownPunctReplacer = strings.NewReplacer(
	"[", "［", // U+FF3B
	"]", "］", // U+FF3D
	"(", "（", // U+FF08
	")", "）", // U+FF09
)

// EscapeMarkdownPunct replaces the markdown link-syntax characters `[`, `]`,
// `(`, `)` with full-width visually-similar codepoints (U+FF3B / U+FF3D /
// U+FF08 / U+FF09) so an attacker-controlled cron Title or result body cannot
// smuggle `[text](url)` clickable links into an IM notice. R260528-SEC-8.
//
// An IndexAny fast-path avoids any allocation on the common ASCII-clean case;
// when substitution is required the Replacer performs exactly one scan + one
// output allocation. R164930-PERF-4/5.
func EscapeMarkdownPunct(s string) string {
	if !strings.ContainsAny(s, "[]()") {
		return s
	}
	return cronMarkdownPunctReplacer.Replace(s)
}
