package sessionview

import (
	"log/slog"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MaxPlannerPromptBytesAtSpawn caps the planner prompt that flows into
// `--append-system-prompt` argv. Mirrors internal/project.MaxPlannerPromptBytes
// (this leaf cannot import project); bounds a tampered on-disk config that
// slipped past the write-path validator.
const MaxPlannerPromptBytesAtSpawn = 8 * 1024

// SanitisePlannerPromptForSpawn is defense-in-depth: re-validate the
// PlannerPrompt at the spawn boundary before it crosses into CLI argv. Returns
// "" for rejected input so the spawn runs with no planner prompt rather than a
// poisoned one. Mirrors project.EffectivePlannerPrompt's rune guards plus a
// length cap so any path bypassing the project layer still cannot inject
// control bytes or oversize argv. Exported for internal/session's
// KeyResolver, which applies it on every planner spawn (#535).
func SanitisePlannerPromptForSpawn(prompt, projectName string) string {
	if prompt == "" {
		return ""
	}
	if len(prompt) > MaxPlannerPromptBytesAtSpawn {
		slog.Warn("planner prompt exceeds spawn-time length cap; dropping",
			"project", projectName,
			"len", len(prompt),
			"cap", MaxPlannerPromptBytesAtSpawn)
		return ""
	}
	if !utf8.ValidString(prompt) {
		slog.Warn("planner prompt contains invalid UTF-8 at spawn; dropping",
			"project", projectName)
		return ""
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		// tab / LF / CR are legitimate markdown; other C0 + NUL + DEL would
		// truncate argv on execve or corrupt stream-json framing at the shim.
		if c == 0 || (c < 0x20 && c != 0x09 && c != 0x0a && c != 0x0d) || c == 0x7f {
			slog.Warn("planner prompt contains control byte at spawn; dropping",
				"project", projectName, "byte", c)
			return ""
		}
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			slog.Warn("planner prompt contains injection rune (C1/bidi/LS-PS) at spawn; dropping",
				"project", projectName)
			return ""
		}
	}
	return prompt
}
