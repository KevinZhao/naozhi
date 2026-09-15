package cli

// The Diag type and the emit/observe machinery live in internal/spawndiag,
// below this package, so internal/envpolicy — a dependency of cli — can report
// its own drops. The names here are the historical ones every caller uses.

import (
	"fmt"
	"strings"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// SpawnDiag is one gate decision that altered or ignored configured input.
type SpawnDiag = spawndiag.Diag

// EmitSpawnDiags logs and counts diags under scope (the session key on spawn
// paths, "config" for load-time diags).
func EmitSpawnDiags(scope string, diags []SpawnDiag) { spawndiag.Emit(scope, diags) }

// ObserveSpawnDiags installs fn as the process-wide diag observer and returns a
// restore func.
func ObserveSpawnDiags(fn func(scope string, d SpawnDiag)) (restore func()) {
	return spawndiag.Observe(fn)
}

// SpawnDiagsFor derives the gate decisions BuildArgs will make for opts — the
// argv-denylist strips (same predicate filterDeniedFlags applies), the argv
// validator refusing a malformed dedicated-field value, and the capability gate
// ignoring an effort tier the backend cannot honour. Pure: no logging, no
// metrics; callers pass the result to EmitSpawnDiags on real spawn paths and to
// the session snapshot.
func SpawnDiagsFor(opts SpawnOptions, caps Caps) []SpawnDiag {
	var diags []SpawnDiag
	if _, over := extraArgsOverCap(opts.ExtraArgs); over {
		diags = append(diags, SpawnDiag{
			Layer:  "argv-denylist",
			Key:    "args",
			Action: "dropped",
			Reason: "ExtraArgs exceeds the argv byte cap; the whole slice is dropped",
		})
		// The cap drops everything; per-flag strips below would be noise.
		return diags
	}
	seen := map[string]bool{}
	for _, a := range opts.ExtraArgs {
		if !isDeniedFlag(a) {
			continue
		}
		name := a
		if eq := strings.IndexByte(a, '='); eq > 0 {
			name = a[:eq]
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		diags = append(diags, SpawnDiag{
			Layer:  "argv-denylist",
			Key:    name,
			Action: "dropped",
			Reason: "flag is denied in ExtraArgs; wire it through its dedicated config field",
		})
	}
	diags = append(diags, argvValidatorDiags(opts)...)
	if opts.Effort != "" && !caps.EffortTier {
		diags = append(diags, SpawnDiag{
			Layer:  "caps",
			Key:    "effort",
			Action: "ignored",
			Reason: "backend does not support a thinking-effort tier",
		})
	}
	return diags
}

// argvValidatorDiags reports the dedicated SpawnOptions fields
// ClaudeProtocol.BuildArgs will refuse to render. Each case calls the same
// predicate the builder does, so the drop and its diagnostic cannot disagree.
// Values are never echoed: a
// ResumeID is attacker-influenced, an AppendSystemPrompt can be long, and both
// would turn a Reason into a log-flooding amplifier — the reasons carry a
// length and a short prefix instead.
func argvValidatorDiags(opts SpawnOptions) []SpawnDiag {
	var diags []SpawnDiag
	if opts.ResumeID != "" && !validResumeID(opts.ResumeID) {
		diags = append(diags, SpawnDiag{
			Layer:  "argv-validator",
			Key:    "--resume",
			Action: "dropped",
			Reason: fmt.Sprintf("resume id is malformed (len %d, prefix %q); spawning a fresh session instead",
				len(opts.ResumeID), resumeIDPreview(opts.ResumeID)),
		})
	}
	if opts.DebugFile != "" && !renderablePathValue(opts.DebugFile) {
		diags = append(diags, SpawnDiag{
			Layer:  "argv-validator",
			Key:    "--debug-file",
			Action: "dropped",
			Reason: "debug file path must be absolute and must not start with '-'; spawning without CLI debug capture",
		})
	}
	if opts.MCPConfigFile != "" && !renderablePathValue(opts.MCPConfigFile) {
		diags = append(diags, SpawnDiag{
			Layer:  "argv-validator",
			Key:    "--mcp-config",
			Action: "dropped",
			Reason: "mcp config path must be absolute and must not start with '-'; spawning without the configured MCP servers",
		})
	}
	if opts.AppendSystemPrompt != "" {
		if reason := invalidAppendSystemPrompt(opts.AppendSystemPrompt); reason != "" {
			diags = append(diags, SpawnDiag{
				Layer:  "argv-validator",
				Key:    "--append-system-prompt",
				Action: "dropped",
				Reason: fmt.Sprintf("system prompt rejected (%s, len %d); spawning without it", reason, len(opts.AppendSystemPrompt)),
			})
		}
	}
	return diags
}
