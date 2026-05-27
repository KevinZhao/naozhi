package server

import (
	"fmt"
	"path/filepath"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
)

// This file holds the cron-handler-edge validation helpers extracted from
// dashboard_cron.go (#1281). The split is purely mechanical — the package
// surface, struct names, and exported behaviour are unchanged. Validation
// state co-located here so future safety changes touch one file instead of
// scrolling past handler bodies.

// Bounds for notify target fields set by authenticated dashboard users. The
// platform must match a known IM provider to avoid silent notification drops
// (misspelt names used to fall through); chat_id length is capped so a user
// cannot stuff megabytes into cron_jobs.json via a single API call.
var validNotifyPlatforms = map[string]struct{}{
	"":        {}, // empty = fall back to cron.notify_default
	"feishu":  {},
	"slack":   {},
	"discord": {},
	"weixin":  {},
}

const maxNotifyChatIDLen = 256

// Cron input bounds shared with the IM `/cron` path. Both surfaces feed the
// same on-disk cron_jobs.json schema, so the limits must stay in lockstep —
// see internal/cron/limits.go. R216-CR-1.
const (
	maxCronPromptBytesDashboard   = cron.MaxPromptBytes
	maxCronIDLenDashboard         = cron.MaxIDLen
	maxCronScheduleBytesDashboard = cron.MaxScheduleBytes
)

// maxCronWorkDirBytesDashboard caps the raw work_dir string before it reaches
// validateWorkspace. Even absolute paths rarely exceed 1 KiB on Linux
// (PATH_MAX is typically 4096), so 1024 is generous. Without this guard a
// multi-MB work_dir body would be echoed into slog attrs via the debug-log
// on validation failure, allowing log-flood from an authenticated attacker.
const maxCronWorkDirBytesDashboard = 1024

// stringFieldPolicy carries the per-field knobs for validateStringField:
// what to call this field in error messages, whether Tab/LF are accepted,
// and whether the three failure classes ("invalid characters" / "invalid
// control characters" / "invalid unicode control characters") collapse
// into a single error label. Centralising these knobs keeps the security
// policy in one place — every cron-edge field shares the same UTF-8 + C0
// + IsLogInjectionRune three-pass scan, so a future safety change touches
// validateStringField alone instead of five copy-pasted loops. R219-CR-5.
type stringFieldPolicy struct {
	// name is the wire-visible field label embedded in error messages.
	name string
	// allowTab whitelists 0x09 in the byte scan (cron prompt / title body).
	allowTab bool
	// allowLF whitelists 0x0a in the byte scan (cron prompt only — cron
	// schedules and absolute paths cannot legally contain a newline).
	allowLF bool
	// disallowLF reports LF / CR as "<name> must be a single line" instead
	// of folding them into the generic "invalid control characters" branch.
	// Used by single-line fields (Job.Title) where the UI specifically
	// requires "no embedded newline" and benefits from a distinct error
	// message rather than a generic control-character class. Mutually
	// exclusive with allowLF — setting both is a programmer error and
	// allowLF wins (LF is whitelisted, disallowLF cannot fire). R239-CR-4.
	disallowLF bool
	// collapseErrors maps every failure class onto "<name> contains invalid
	// characters" instead of the WorkDir/Prompt-style three-tier messages.
	// True for notify_chat_id and schedule (where API consumers historically
	// only see one error string); false for work_dir / prompt where the
	// distinction between "control byte" and "bidi rune" carries audit
	// signal.
	collapseErrors bool
}

// validateStringField runs the three-pass UTF-8 → C0+DEL byte → log-injection
// rune scan that every cron-handler-edge user-controlled string requires.
// The caller owns the length check (units differ: WorkDir/Prompt cap bytes,
// Title caps runes, NotifyChatID caps bytes) and any field-specific extras
// (validateCronWorkDir's filepath.IsAbs check). R219-CR-5.
//
// R250-PERF-22 (#1125): the IsLogInjectionRune set covers C1 (0x80..0x9F)
// and assorted Unicode formatting codepoints (bidi, LS/PS) which all
// encode in UTF-8 with at least one byte >= 0x80. An ASCII-only string —
// the common case for absolute paths, schedule expressions, lowercase-hex
// IDs — therefore cannot contain a hit, and the second `for _, r := range
// s` decode pass is pure overhead. Track an `anyHighBit` flag during the
// first byte loop and skip the rune walk when the input is provably
// ASCII. Hot path on cron CREATE/PATCH validates 5+ fields per request.
func validateStringField(s string, p stringFieldPolicy) error {
	// R179-GO-P1: validate UTF-8 before the rune-range loop below. A
	// `for _, r := range s` over broken UTF-8 silently produces utf8.RuneError
	// (U+FFFD) for each invalid byte, which IsLogInjectionRune does not flag
	// — this lets a crafted string with lone continuation bytes smuggle
	// arbitrary bytes into cron_jobs.json / WS broadcasts. Mirrors
	// validateProjectName.
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s contains invalid characters", p.name)
	}
	anyHighBit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			anyHighBit = true
			continue
		}
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' && p.allowTab {
			continue
		}
		if c == '\n' && p.allowLF {
			continue
		}
		// R239-CR-4: surface single-line violations (LF / CR) with a
		// distinct error message when disallowLF is set, instead of
		// folding them into the generic control-character class. UI
		// fields like Job.Title surface this directly to the operator.
		if p.disallowLF && (c == '\n' || c == '\r') {
			return fmt.Errorf("%s must be a single line", p.name)
		}
		if p.collapseErrors {
			return fmt.Errorf("%s contains invalid characters", p.name)
		}
		return fmt.Errorf("%s contains invalid control characters", p.name)
	}
	// R250-PERF-22 (#1125): pure-ASCII fast path — IsLogInjectionRune cannot
	// match any rune whose UTF-8 form is single-byte < 0x80, so the rune
	// loop below has zero work to do when no high-bit byte was observed.
	// Skips the second UTF-8 decode pass on the common case (absolute
	// paths, cron schedules, lowercase-hex IDs).
	if !anyHighBit {
		return nil
	}
	// Reject Unicode bidi override / embedding / directional isolate
	// characters (U+202A–U+202E, U+2066–U+2069) and Unicode line/paragraph
	// separators (U+2028/U+2029) which encode as valid UTF-8 sequences with
	// all bytes >= 0x20 and therefore pass the byte loop above. These
	// characters can flip terminal rendering and corrupt log pipelines that
	// use U+2028 as a line boundary. Matches the filter applied by
	// sanitizeKeyComponent in the session package so cron fields and
	// session-key fields reject the same log-injection class uniformly.
	for _, r := range s {
		if osutil.IsLogInjectionRune(r) {
			if p.collapseErrors {
				return fmt.Errorf("%s contains invalid characters", p.name)
			}
			return fmt.Errorf("%s contains invalid unicode control characters", p.name)
		}
	}
	return nil
}

// validateCronWorkDir rejects work_dir strings with embedded control
// characters that would corrupt slog attribute logging (ANSI injection into
// structured logs, CR/LF line-wrapping into log pipelines). Length check
// matches prompt/schedule guards so all three fields reject the same class
// of log-injection payloads at the handler edge, before validateWorkspace
// sees them.
//
// R172-SEC-L1: relative paths are rejected up front so the cron edge
// boundary does not depend on validateWorkspace to fail on "." / "foo/bar"
// later. Defense-in-depth: if validateWorkspace ever loosens its IsAbs
// check (e.g. to accept workspace-relative paths for a new feature) the
// cron handler continues to enforce the stricter contract inherited from
// the scheduler worker which runs on absolute paths only.
func validateCronWorkDir(wd string) error {
	if len(wd) > maxCronWorkDirBytesDashboard {
		return fmt.Errorf("work_dir exceeds %d-byte limit", maxCronWorkDirBytesDashboard)
	}
	if err := validateStringField(wd, stringFieldPolicy{name: "work_dir"}); err != nil {
		return err
	}
	if !filepath.IsAbs(wd) {
		return fmt.Errorf("work_dir must be an absolute path")
	}
	return nil
}

// validateNotifyTarget enforces platform allowlist + chat_id size bound.
// R177-SEC-7: additionally reject C0/C1/bidi/LS/PS runes so a crafted
// chat_id cannot land log-injection bytes in persisted cron_jobs.json
// or forge structure in the /api/cron WS broadcast.
func validateNotifyTarget(platform, chatID string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	if len(chatID) > maxNotifyChatIDLen {
		return fmt.Errorf("notify_chat_id exceeds %d-byte limit", maxNotifyChatIDLen)
	}
	return validateStringField(chatID, stringFieldPolicy{name: "notify_chat_id", collapseErrors: true})
}

// validateCronScheduleChars rejects C0/C1/bidi/LS/PS runes in a cron
// schedule expression before it reaches robfig/cron's parser. robfig
// does not scrub its input, so log lines like `slog.Debug("cron
// preview parse failed", "err", err)` would forward unescaped bidi
// overrides into operator logs. Authenticated-only endpoint so the
// CVSS is low, but this keeps the log-injection posture consistent
// across every user-controlled string entering scheduler paths.
// R177-SEC-9.
func validateCronScheduleChars(schedule string) error {
	return validateStringField(schedule, stringFieldPolicy{name: "schedule", collapseErrors: true})
}

// validateCronBackend enforces the shared shape contract for the
// dashboard-picked backend override on cron jobs:
//   - empty is OK (router default fallback at execute time);
//   - length <= maxBackendIDLen bytes;
//   - charset matches isValidBackendID (R233-SEC-9 unification).
//
// Unknown backend IDs are NOT rejected here — the session router's
// wrapperFor clamps unknowns to the configured default so the cron job
// keeps running rather than failing every tick because the operator
// removed a backend from config.yaml. This handler-edge gate stops only
// shape-invalid input that would otherwise pollute logs / persisted JSON.
//
// R233-SEC-9: previously used a tighter [a-z0-9_-] subset, leaving
// uppercase / '.' allowed by the WS isValidBackendID path and rejected
// here. Now both paths share isValidBackendID + maxBackendIDLen so a
// caller cannot confuse the two surfaces. The relaxation is forward-
// compatible: any backend ID validated under the older subset still
// satisfies isValidBackendID's superset.
func validateCronBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendIDLen {
		return fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
	}
	if !isValidBackendID(backend) {
		// R230-CQ-12: error string aligned with send.handleSend's
		// dashboard-side gate so dashboard JS / external API consumers
		// see one message regardless of which surface rejected.
		return fmt.Errorf("invalid backend identifier")
	}
	return nil
}

// validateCronPrompt rejects prompts larger than the dashboard cap or
// containing control characters. Cron prompts are delivered via stdin as a
// stream-json user message (cron/scheduler.go → session.Send → NewUserMessage),
// where json.Marshal escapes embedded \n so NDJSON framing stays intact. LF is
// therefore allowed to support multi-paragraph playbook prompts. CR is still
// rejected because `tail -f` / `journalctl` treat it as a carriage return that
// overwrites the current log line — a log-poisoning surface unrelated to
// framing. null bytes remain forbidden (execve silently truncates at the first
// NUL). Tab is allowed because prompts may indent examples.
//
// Unlike project_api.handleConfigPut's planner_prompt guard, cron prompts do
// not end up in argv — planner_prompt and scratch context still flow into
// `--append-system-prompt` and must stay single-line; do not copy this relaxed
// policy back to those fields without re-auditing their downstream writers.
//
// Second pass mirrors validateCronWorkDir: reject C1 controls + Unicode
// bidi / directional isolate / line separator runes that are >= 0x20 at
// the byte level and therefore bypass the ASCII loop above.
// validateCronTitle 是 Job.Title 在 handler 层的守门：单行（禁内嵌换行，
// 卡片布局不允许）、长度 256 rune、禁控制字符 + 日志注入 rune。空值合法
// （允许用户不填，UI 自动 fallback 到 Prompt 首行）。
// 与 validateCronPrompt 一致的清洗集，只多禁换行。
//
// R239-CR-4: 通过 stringFieldPolicy{disallowLF: true} 复用 validateStringField
// 共享的 25 行 C0+IsLogInjectionRune 扫描，不再维护独立 loop。Tab 仍 allow，
// 长度仍按 rune 计（与 validateStringField 的 byte 长度独立处理在外层）。
func validateCronTitle(title string) error {
	if title == "" {
		return nil
	}
	if n := utf8.RuneCountInString(title); n > cron.MaxCronTitleLen {
		return fmt.Errorf("title exceeds %d-rune limit", cron.MaxCronTitleLen)
	}
	return validateStringField(title, stringFieldPolicy{name: "title", allowTab: true, disallowLF: true})
}

// validateCronPrompt allows Tab and LF (multi-paragraph playbooks) but
// stringFieldPolicy with allowLF=true still rejects \r (CR). That asymmetry
// is intentional: prompts are written into cron_jobs.json as JSON-quoted
// strings (json.Marshal escapes \n inside the quoted value, so NDJSON
// framing on the wire stays intact), but a bare CR would still survive the
// JSON encode and later corrupt `tail -f` / `journalctl` views by carriage-
// returning over the previous log line — a log-poisoning surface unrelated
// to wire framing. There is no legitimate reason for an authored prompt to
// contain CR (Linux line endings are LF, dashboard textareas normalise
// CRLF→LF before submit), so rejecting it here is cheap defence-in-depth
// matching validateCronTitle's explicit '\r' branch. R230-CQ-19.
func validateCronPrompt(prompt string) error {
	if len(prompt) > maxCronPromptBytesDashboard {
		return fmt.Errorf("prompt exceeds %d-byte limit", maxCronPromptBytesDashboard)
	}
	return validateStringField(prompt, stringFieldPolicy{name: "prompt", allowTab: true, allowLF: true})
}
