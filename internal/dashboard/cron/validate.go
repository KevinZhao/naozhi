package cron

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	cronpkg "github.com/naozhi/naozhi/internal/cron"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Notify target bounds: platform must be a known IM provider (misspelt names
// would silently drop notifications); chat_id length is capped so one request
// cannot bloat cron_jobs.json.
var validNotifyPlatforms = map[string]struct{}{
	"":        {}, // empty = fall back to cron.notify_default
	"feishu":  {},
	"slack":   {},
	"discord": {},
	"weixin":  {},
}

const maxNotifyChatIDLen = 256

// maxCronWorkDirBytesDashboard caps raw work_dir before validateWorkspace so a
// multi-MB body is not echoed into slog attrs on failure (log-flood).
const maxCronWorkDirBytesDashboard = 1024

// stringFieldPolicy carries the per-field knobs for validateStringField so every
// cron-edge field shares one UTF-8 + C0 + IsLogInjectionRune scan.
type stringFieldPolicy struct {
	name string
	// allowTab whitelists 0x09 (cron prompt / title body).
	allowTab bool
	// allowLF whitelists 0x0a (cron prompt only; schedules / paths never contain LF).
	allowLF bool
	// disallowLF reports LF / CR as "<name> must be a single line" (Job.Title).
	// Mutually exclusive with allowLF; if both are set allowLF wins.
	disallowLF bool
	// collapseErrors folds every failure class into "<name> contains invalid
	// characters"; work_dir / prompt keep the three-tier messages for audit signal.
	collapseErrors bool
}

// validateStringField runs the UTF-8 → C0+DEL → log-injection rune scan every
// user-controlled cron string requires; callers own the length check and
// field-specific extras. Every IsLogInjectionRune hit has a byte >= 0x80, so a
// provably-ASCII input skips the rune walk (#1125).
func validateStringField(s string, p stringFieldPolicy) error {
	// Validate UTF-8 first: ranging over broken UTF-8 yields U+FFFD, which
	// IsLogInjectionRune does not flag, letting lone continuation bytes smuggle
	// arbitrary bytes into cron_jobs.json / WS broadcasts.
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
		if p.disallowLF && (c == '\n' || c == '\r') {
			return fmt.Errorf("%s must be a single line", p.name)
		}
		if p.collapseErrors {
			return fmt.Errorf("%s contains invalid characters", p.name)
		}
		return fmt.Errorf("%s contains invalid control characters", p.name)
	}
	if !anyHighBit {
		return nil
	}
	// Reject bidi overrides / isolates (U+202A–U+202E, U+2066–U+2069) and LS/PS
	// (U+2028/U+2029): valid UTF-8 with all bytes >= 0x20, so they pass the byte
	// loop, yet they flip terminal rendering and break log pipelines.
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

// validateCronWorkDir rejects oversized / control-character work_dir strings at
// the handler edge (log injection) and relative paths as defense-in-depth: the
// scheduler worker runs on absolute paths only.
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

// validateNotifyTarget enforces the platform allowlist, chat_id size bound and
// log-injection rune scan (chat_id lands in cron_jobs.json and WS broadcasts).
func validateNotifyTarget(platform, chatID string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	if len(chatID) > maxNotifyChatIDLen {
		return fmt.Errorf("notify_chat_id exceeds %d-byte limit", maxNotifyChatIDLen)
	}
	return validateStringField(chatID, stringFieldPolicy{name: "notify_chat_id", collapseErrors: true})
}

// validateCronScheduleChars rejects log-injection runes before the schedule
// reaches robfig/cron, whose parser errors are forwarded into operator logs.
func validateCronScheduleChars(schedule string) error {
	// Shared with the IM dispatch.ParseCronAdd edge so policies cannot drift (#1315).
	return cronpkg.ValidateScheduleChars(schedule)
}

// ValidateCronBackend enforces the shape contract for the dashboard-picked
// backend override: empty OK, length <= maxBackendIDLen, charset per
// isValidBackendID (shared with the WS path). Unknown backend IDs are NOT
// rejected — the router's wrapperFor clamps them to the default so a job keeps
// running after an operator removes a backend from config.yaml.
func ValidateCronBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendIDLen {
		return fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
	}
	if !isValidBackendID(backend) {
		// Same error string as send.handleSend's gate.
		return fmt.Errorf("invalid backend identifier")
	}
	return nil
}

// validateCronPlacement gates placement at the HTTP edge: value shape
// (""/"local"/"sandbox") and the Phase 1 sandbox guardrail (no work_dir until
// clone-on-boot lands, RFC §4.4 B10-a). The scheduler re-validates; this copy
// gives the dashboard a precise 400 instead of a generic save error.
func validateCronPlacement(placement, workDir string) error {
	switch placement {
	case "", cronpkg.PlacementLocal:
		return nil
	case cronpkg.PlacementSandbox:
		if workDir != "" {
			return fmt.Errorf("云沙箱暂不支持工作目录（Phase 1）：请清空 work_dir 或改用本机运行")
		}
		return nil
	default:
		return fmt.Errorf("invalid placement %q", placement)
	}
}

// validateCronTitle 是 Job.Title 在 handler 层的守门：单行、长度 256 rune、禁控制
// 字符 + 日志注入 rune；空值合法（UI fallback 到 Prompt 首行）。
// 通过 stringFieldPolicy{disallowLF: true} 复用 validateStringField。
func validateCronTitle(title string) error {
	if title == "" {
		return nil
	}
	if n := utf8.RuneCountInString(title); n > cronpkg.MaxCronTitleLen {
		return fmt.Errorf("title exceeds %d-rune limit", cronpkg.MaxCronTitleLen)
	}
	return validateStringField(title, stringFieldPolicy{name: "title", allowTab: true, disallowLF: true})
}

// validateCronPrompt delegates the shared scan to cronpkg.ValidatePromptStrict
// (one policy with the IM `/cron` edge, #1188) and adds one dashboard-only
// rule: reject CR. LF is safe (JSON-quoted in cron_jobs.json) but a bare CR
// survives the encode and carriage-returns over the previous line in
// `tail -f` / `journalctl` — a log-poisoning surface.
func validateCronPrompt(prompt string) error {
	if err := cronpkg.ValidatePromptStrict(prompt); err != nil {
		return err
	}
	if strings.ContainsRune(prompt, '\r') {
		return fmt.Errorf("prompt contains invalid control characters")
	}
	return nil
}
