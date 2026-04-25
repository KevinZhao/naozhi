package project

import (
	"errors"
	"fmt"
	"regexp"
)

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
	return nil
}
