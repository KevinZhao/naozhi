package config

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MaxAgentSystemPromptBytes caps agents[<id>].system_prompt: larger than the
// planner-prompt cap, yet with a full scratch block (24 KiB) still inside
// cliinfo.MaxAppendSystemPromptBytes (64 KiB).
const MaxAgentSystemPromptBytes = 32 * 1024

// validateSystemPrompt gates agents[<id>].system_prompt. Unlike
// validatePlannerPrompt it ALLOWS LF (multi-line block scalars reach the shim
// as one argv element, where LF is inert); NUL, other C0 incl. CR, DEL, C1,
// bidi, LS/PS and a leading '-' (BuildArgs would drop the whole prompt as
// flag injection) are rejected at load.
func validateSystemPrompt(field, prompt string) error {
	if prompt == "" {
		return nil
	}
	if len(prompt) > MaxAgentSystemPromptBytes {
		return fmt.Errorf("%s exceeds %d-byte limit", field, MaxAgentSystemPromptBytes)
	}
	if strings.HasPrefix(prompt, "-") {
		return fmt.Errorf("%s must not start with '-' (would be parsed as a CLI flag)", field)
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c == 0 || (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			return fmt.Errorf("%s contains invalid control characters (NUL/C0/DEL — argv corruption guard; only TAB and LF are allowed)", field)
		}
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			return fmt.Errorf("%s contains invalid unicode controls (C1/bidi/LS-PS)", field)
		}
	}
	return nil
}
