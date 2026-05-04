package project

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MaxProjectNameBytes caps project-name inputs that traverse trust
// boundaries (dashboard query, reverse-RPC frames). 128 bytes matches
// the session-key component cap and is well over any realistic
// directory name under projects_root.
const MaxProjectNameBytes = 128

// ValidateProjectName rejects oversized / control-character names before
// they flow into slog attrs or map lookups. Mirrors the validator that
// already gates the dashboard /api/projects path; this exported form is
// reused by the reverse-RPC worker so both trust boundaries share one
// policy. R181-SEC-P2-2.
//
// The policy is intentionally conservative: directory names under
// projects_root are unlikely to contain C0/C1/bidi runes legitimately,
// and accepting them would let a compromised primary forge log entries
// or flip the bidi of any dashboard that renders the name.
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
	return nil
}

// maxPlannerPromptBytes is the hard cap on PlannerPrompt size. An
// oversized prompt would inflate the exec.Command argv past Linux's
// ARG_MAX (~2 MB) and make Spawn fail with a cryptic E2BIG.
const maxPlannerPromptBytes = 8 * 1024

// maxPlannerModelBytes is the hard cap on PlannerModel length.
const maxPlannerModelBytes = 256

// plannerModelRe restricts the model identifier to safe characters so a
// crafted value cannot sneak extra CLI flags (e.g. " --dangerously-skip-permissions")
// into the exec.Command argv for the planner CLI. Whitespace, dashes at the
// start, and control characters are rejected.
var plannerModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\-]*$`)

// ErrInvalidConfig is returned when ValidateConfig rejects untrusted input.
// Callers should map it to an HTTP 400 or RPC client error.
var ErrInvalidConfig = errors.New("invalid project config")

// ValidateConfig enforces the same safety checks on ProjectConfig regardless
// of ingress path (HTTP dashboard PUT vs reverse-RPC update_config from a
// primary node). Both paths must reject:
//
//   - PlannerPrompt over maxPlannerPromptBytes
//   - PlannerPrompt containing C0 control bytes other than tab, or DEL.
//     Null bytes silently truncate argv on execve; raw \n / \r corrupt
//     NDJSON protocol framing at the shim boundary.
//   - PlannerModel over maxPlannerModelBytes
//   - PlannerModel failing plannerModelRe (flag-injection guard)
//
// Returning ErrInvalidConfig wrapped with a human-readable reason keeps
// the public error stable while still surfacing the specific field for
// operator logs. R68-SEC-H2.
func ValidateConfig(cfg ProjectConfig) error {
	if len(cfg.PlannerPrompt) > maxPlannerPromptBytes {
		return fmt.Errorf("%w: planner_prompt exceeds %d-byte limit", ErrInvalidConfig, maxPlannerPromptBytes)
	}
	for i := 0; i < len(cfg.PlannerPrompt); i++ {
		c := cfg.PlannerPrompt[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return fmt.Errorf("%w: planner_prompt contains invalid control characters", ErrInvalidConfig)
		}
	}
	if len(cfg.PlannerModel) > maxPlannerModelBytes {
		return fmt.Errorf("%w: planner_model exceeds %d-byte limit", ErrInvalidConfig, maxPlannerModelBytes)
	}
	if cfg.PlannerModel != "" && !plannerModelRe.MatchString(cfg.PlannerModel) {
		return fmt.Errorf("%w: planner_model contains invalid characters", ErrInvalidConfig)
	}
	// R184-SEC-M1: reject ChatBindings fields that would break the
	// bindingIndex key invariant "platform:chatType:chatID". A colon in any
	// component would collide with an entirely different (platform,chatType,
	// chatID) triple and silently reroute messages; a NUL byte truncates
	// argv/YAML parsers unpredictably. Size caps prevent an attacker-crafted
	// config from stuffing multi-KB strings into the in-memory index.
	for i, b := range cfg.ChatBindings {
		// R185-SEC-M2: empty required fields pollute bindingIndex with
		// nonsense keys like ":group:oc_xxx" or "feishu:group:" that can
		// never legitimately match a real event, but bloat every rebuild.
		if b.Platform == "" || b.ChatType == "" || b.ChatID == "" {
			return fmt.Errorf("%w: chat_bindings[%d] has empty required field", ErrInvalidConfig, i)
		}
		if err := validateBindingField(b.Platform, b.ChatType, b.ChatID); err != nil {
			return fmt.Errorf("%w: chat_bindings[%d]: %s", ErrInvalidConfig, i, err.Error())
		}
	}
	return nil
}

// validateBindingField enforces the invariants for a single ChatBinding:
//   - no ':' (would collide with a different platform:chatType:chatID triple
//     in bindingIndex and silently misroute messages)
//   - no NUL (truncates argv / YAML parsers unpredictably)
//   - size caps to prevent a crafted config bloating the in-memory index
//
// Shared by ValidateConfig (dashboard PUT + reverse-RPC update_config) and
// BindChat (IM /project command path) so every ingress trips the same guard.
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
