package config

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MaxAgentSystemPromptBytes caps agents[<id>].system_prompt. Larger than the
// 8 KiB planner-prompt cap (project.MaxPlannerPromptBytes) because an agent's
// standing instructions are legitimately longer than a one-line project
// preamble, yet small enough that stacking it with a full scratch context
// block (24 KiB) stays well inside cli.MaxAppendSystemPromptBytes (64 KiB).
const MaxAgentSystemPromptBytes = 32 * 1024

// legacySystemPromptFlag is the CLI flag operators used to (ineffectively)
// write under agents[].args before agents[].system_prompt existed (#2493).
const legacySystemPromptFlag = "--append-system-prompt"

// validateSystemPrompt gates agents[<id>].system_prompt. Empty is accepted
// (no prompt). Unlike validatePlannerPrompt it ALLOWS LF — a system prompt is
// naturally a multi-line YAML block scalar, and the value is passed to the
// shim as one execve argv element (`--cli-arg <value>`), where LF is inert.
// Everything else follows the argv-corruption / log-injection policy shared
// with the planner prompt: NUL and the remaining C0 controls (incl. CR), DEL,
// C1 controls, bidi overrides/isolates and LS/PS are rejected, as is a
// leading '-' (cli.BuildArgs would drop the whole prompt as a flag-injection
// attempt — better to tell the operator at load).
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

// liftLegacySystemPromptArgs migrates `--append-system-prompt <p>` (or the
// `--append-system-prompt=<p>` form) found under agents[<id>].args into
// agents[<id>].system_prompt, removing it from args, and warns with the field
// path so the operator can move it in the file.
//
// Why lift instead of reject: the flag under args never worked (#2493 — it
// was denylisted and stripped at every spawn), so the operator's intent was
// already being lost silently. Rejecting would turn the fix into a startup
// failure on the next upgrade for exactly the configs the fix is meant to
// repair; lifting makes them work and says so. Several occurrences are joined
// "\n\n" in order. An explicit system_prompt alongside the legacy flag is a
// conflict and is rejected — there is no right answer for which wins.
//
// Only agents[].args is lifted: cli.args / cli.backends[].args have no
// per-backend system-prompt field to lift into, so there the flag is
// reported by validateArgvStrings' denied-flag warning instead.
func liftLegacySystemPromptArgs(cfg *Config) error {
	for id, ac := range cfg.Agents {
		kept, lifted, ok := splitLegacySystemPromptArgs(ac.Args)
		if !ok {
			continue
		}
		field := fmt.Sprintf("agents[%s]", id)
		if lifted == "" {
			// Bare trailing flag with no value: nothing to lift, but the
			// dangling token must not stay in args either.
			slog.Warn("config: dropped bare "+legacySystemPromptFlag+" with no value",
				"field", field+".args")
		} else if ac.SystemPrompt != "" {
			return fmt.Errorf("%s: both system_prompt and %s in args are set; remove the args entry (system_prompt is the supported field)",
				field, legacySystemPromptFlag)
		} else {
			slog.Warn("config: "+legacySystemPromptFlag+" under args is not applied at spawn (denied flag); lifted into system_prompt — please move it in config.yaml",
				"field", field+".args", "target", field+".system_prompt", "bytes", len(lifted))
		}
		next := ac // copy; never mutate the map value in place
		next.Args = kept
		if lifted != "" {
			next.SystemPrompt = lifted
		}
		cfg.Agents[id] = next
	}
	return nil
}

// splitLegacySystemPromptArgs returns args without any legacySystemPromptFlag
// occurrences (both `--flag value` and `--flag=value` forms, mirroring
// cli.filterDeniedFlags' shape rules), the joined prompt values, and whether
// anything was removed. kept aliases args when found=false.
func splitLegacySystemPromptArgs(args []string) (kept []string, lifted string, found bool) {
	for _, a := range args {
		if a == legacySystemPromptFlag || strings.HasPrefix(a, legacySystemPromptFlag+"=") {
			found = true
			break
		}
	}
	if !found {
		return args, "", false
	}
	kept = make([]string, 0, len(args))
	var vals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, isEq := strings.CutPrefix(a, legacySystemPromptFlag+"="); isEq {
			vals = append(vals, v)
			continue
		}
		if a == legacySystemPromptFlag {
			// Same rule as cli.filterDeniedFlags: the next token is the value
			// unless it looks like another flag.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				vals = append(vals, args[i+1])
				i++
			}
			continue
		}
		kept = append(kept, a)
	}
	if len(kept) == 0 {
		kept = nil
	}
	return kept, strings.Join(vals, "\n\n"), true
}
