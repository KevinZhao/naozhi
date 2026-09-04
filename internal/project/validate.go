package project

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MaxProjectNameBytes caps project-name inputs crossing trust boundaries
// (dashboard query, reverse-RPC frames); matches the session-key component cap.
const MaxProjectNameBytes = 128

// ValidateProjectName rejects oversized / control-character names before they
// flow into slog attrs or map lookups; shared by the dashboard /api/projects
// path and the reverse-RPC worker. Conservative on purpose: accepting
// C0/C1/bidi runes would let a compromised primary forge log entries or flip
// bidi in any dashboard rendering the name.
func ValidateProjectName(name string) error {
	if name == "" {
		return errors.New("project name is required")
	}
	if len(name) > MaxProjectNameBytes {
		return errors.New("project name too long")
	}
	if !utf8.ValidString(name) {
		return errors.New("project name invalid utf-8")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) {
			return errors.New("project name contains invalid characters")
		}
	}
	// ':' is the session-key field delimiter: PlannerKeyFor("foo:planner") ==
	// "project:foo:planner:planner", whose chatKeyFor collapses to project
	// foo's planner key, so messages could route to the wrong project's
	// planner. ':' is already stripped from IM key components, so rejecting
	// it loses nothing.
	if strings.ContainsRune(name, ':') {
		return errors.New("project name must not contain ':'")
	}
	return nil
}

// maxDisplayNameRunes caps the operator-facing label; matches MaxProjectNameBytes.
const maxDisplayNameRunes = 128

// maxEmojiRunes caps the emoji prefix: one emoji with modifiers/ZWJ can reach
// ~4 runes; 8 gives headroom without permitting full words.
const maxEmojiRunes = 8

// MaxPlannerPromptBytes caps PlannerPrompt so argv stays well under Linux
// ARG_MAX (~2 MB, otherwise a cryptic E2BIG). Exported so internal/config
// applies the same cap to projects.planner_defaults.prompt.
const MaxPlannerPromptBytes = 8 * 1024

// maxPlannerModelBytes is the hard cap on PlannerModel length.
const maxPlannerModelBytes = 256

// maxGitRemoteBytes / maxMemoryFileBytes cap the GitRemote URL and MemoryFile
// path (both comfortably under 2 KiB) so a crafted config cannot stuff
// multi-KB strings into the project map or a future exec/file-open argv.
const maxGitRemoteBytes = 2048
const maxMemoryFileBytes = 2048

// plannerModelRe restricts the model identifier so a crafted value cannot
// sneak extra CLI flags (" --dangerously-skip-permissions") into the planner
// argv. `[` `]` admit the CLI's context-window suffix (`…[1m]`), as session.modelRe.
var plannerModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]\-]*$`)

// ErrInvalidConfig is returned when ValidateConfig rejects untrusted input (HTTP 400 / RPC error).
var ErrInvalidConfig = errors.New("invalid project config")

// ValidateConfig enforces the same safety checks on ProjectConfig regardless
// of ingress (HTTP dashboard PUT vs reverse-RPC update_config): PlannerPrompt
// size cap and no C0 controls other than tab / no DEL (NUL truncates argv on
// execve; raw \n / \r corrupt NDJSON framing at the shim boundary);
// PlannerModel size cap and plannerModelRe (flag-injection guard).
func ValidateConfig(cfg ProjectConfig) error {
	if len(cfg.PlannerPrompt) > MaxPlannerPromptBytes {
		return fmt.Errorf("%w: planner_prompt exceeds %d-byte limit", ErrInvalidConfig, MaxPlannerPromptBytes)
	}
	for i := 0; i < len(cfg.PlannerPrompt); i++ {
		c := cfg.PlannerPrompt[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return fmt.Errorf("%w: planner_prompt contains invalid control characters", ErrInvalidConfig)
		}
	}
	// The byte loop catches ASCII C0/DEL; the rune loop catches C1 controls,
	// bidi overrides/isolates and LS/PS, which are >= 0x20 at byte level.
	// PlannerPrompt flows into CLI argv (--append-system-prompt) and slog attrs.
	for _, r := range cfg.PlannerPrompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: planner_prompt contains invalid unicode control characters", ErrInvalidConfig)
		}
	}
	if len(cfg.PlannerModel) > maxPlannerModelBytes {
		return fmt.Errorf("%w: planner_model exceeds %d-byte limit", ErrInvalidConfig, maxPlannerModelBytes)
	}
	if cfg.PlannerModel != "" && !plannerModelRe.MatchString(cfg.PlannerModel) {
		return fmt.Errorf("%w: planner_model contains invalid characters", ErrInvalidConfig)
	}
	// DisplayName / Emoji flow into dashboard HTML and slog attrs: reject
	// C0/C1/bidi/LS-PS like PlannerPrompt.
	if utf8.RuneCountInString(cfg.DisplayName) > maxDisplayNameRunes {
		return fmt.Errorf("%w: display_name exceeds %d-rune limit", ErrInvalidConfig, maxDisplayNameRunes)
	}
	for _, r := range cfg.DisplayName {
		if r == 0 || r == 0x7f || (r < 0x20 && r != '\t') || osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: display_name contains invalid characters", ErrInvalidConfig)
		}
	}
	if utf8.RuneCountInString(cfg.Emoji) > maxEmojiRunes {
		return fmt.Errorf("%w: emoji exceeds %d-rune limit", ErrInvalidConfig, maxEmojiRunes)
	}
	for _, r := range cfg.Emoji {
		if r == 0 || r == 0x7f || r < 0x20 || osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: emoji contains invalid characters", ErrInvalidConfig)
		}
	}
	// GitRemote / MemoryFile arrive via untrusted ingress and are slated to feed
	// exec (git remote) and file-open paths: reject NUL, C0, DEL and the
	// IsLogInjectionRune class, and cap bytes, so no ingress stages argv-truncation.
	if err := validateOpaqueField("git_remote", cfg.GitRemote, maxGitRemoteBytes); err != nil {
		return err
	}
	if err := validateOpaqueField("memory_file", cfg.MemoryFile, maxMemoryFileBytes); err != nil {
		return err
	}
	// Backend / AccessProfile reach CLI argv (--backend) and slog attrs via the
	// same untrusted ingress. Referential validity lives at the config layer;
	// here only length and charset are bounded (no flag-shaped / control bytes).
	if err := validateIdentToken("backend", cfg.Backend); err != nil {
		return err
	}
	if err := validateIdentToken("access_profile", cfg.AccessProfile); err != nil {
		return err
	}
	// ChatBindings must uphold the bindingIndex key invariant
	// "platform:chatType:chatID": a colon collides with a different triple and
	// silently reroutes; NUL truncates argv/YAML parsers; size caps stop a
	// crafted config bloating the index.
	for i, b := range cfg.ChatBindings {
		// Empty required fields pollute bindingIndex with keys that can never
		// match a real event.
		if b.Platform == "" || b.ChatType == "" || b.ChatID == "" {
			return fmt.Errorf("%w: chat_bindings[%d] has empty required field", ErrInvalidConfig, i)
		}
		if err := validateBindingField(b.Platform, b.ChatType, b.ChatID); err != nil {
			return fmt.Errorf("%w: chat_bindings[%d]: %s", ErrInvalidConfig, i, err.Error())
		}
	}
	return nil
}

// validateOpaqueField caps a free-form config field at maxBytes and rejects
// NUL, C0, DEL and the C1/bidi/LS-PS class — the PlannerPrompt content
// policy. GitRemote / MemoryFile may legitimately contain ':' or '/' but
// must not smuggle control bytes into argv, NDJSON or slog. Empty = unset.
func validateOpaqueField(field, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d-byte limit", ErrInvalidConfig, field, maxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid utf-8", ErrInvalidConfig, field)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == 0 || c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: %s contains invalid control characters", ErrInvalidConfig, field)
		}
	}
	for _, r := range value {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%w: %s contains invalid unicode control characters", ErrInvalidConfig, field)
		}
	}
	return nil
}

// identTokenRe matches backend IDs and access-profile names (1-64 chars,
// alphanumerics + `._-`, no leading '-'); matches the backendid charset contract.
var identTokenRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateIdentToken bounds an identifier-shaped config field (backend ID /
// access-profile name). Empty means "default"; field is only for the error.
func validateIdentToken(field, value string) error {
	if value == "" {
		return nil
	}
	if !identTokenRe.MatchString(value) {
		return fmt.Errorf("%w: %s contains invalid characters or is too long", ErrInvalidConfig, field)
	}
	return nil
}

// validateBindingField enforces the ChatBinding invariants: no ':' (collides
// with another platform:chatType:chatID triple in bindingIndex), no NUL, and
// size caps. Shared by ValidateConfig and BindChat so every ingress trips the
// same guard.
func validateBindingField(platform, chatType, chatID string) error {
	if strings.ContainsAny(platform, ":\x00") ||
		strings.ContainsAny(chatType, ":\x00") ||
		strings.ContainsAny(chatID, ":\x00") {
		return errors.New("contains invalid characters (':' or NUL)")
	}
	if len(platform) > 64 || len(chatType) > 64 || len(chatID) > 256 {
		return errors.New("field exceeds length limit")
	}
	return nil
}
