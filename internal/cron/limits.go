package cron

import (
	"errors"
	"fmt"
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
