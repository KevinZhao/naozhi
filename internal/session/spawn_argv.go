package session

import "github.com/naozhi/naozhi/internal/cli"

// argvSpawnOptions builds the SpawnOptions subset that Protocol.BuildArgs turns
// into argv, keeping the real spawn (spawnSession) and the arg-drift comparison
// (driftCompareArgs) bit-identical. Any argv-bearing field set on only one path
// reads as permanent drift and kills every surviving shim on each naozhi
// restart, so the argv inputs are positional parameters: a caller that forgets
// one does not compile. Add new argv-bearing fields HERE, not at a call site.
//
// debugFile is a parameter because the two paths need different side effects
// (spawn pre-creates/hardens via cliDebugFileFor; drift stays read-only via
// cliDebugPathFor). PermissionMode is absent: both paths use the zero value.
func (r *Router) argvSpawnOptions(model, effort, debugFile, systemPrompt string, extraArgs []string) cli.SpawnOptions {
	return cli.SpawnOptions{
		Model:              model,
		Effort:             effort,
		ExtraArgs:          extraArgs,
		DebugFile:          debugFile,
		AppendSystemPrompt: systemPrompt,
		// "" unless the operator opted into naozhi-owned isolated settings
		// (RFC naozhi-owned-settings-v3); only ClaudeProtocol acts on it.
		SettingsFile: r.naozhiSettingsFile,
		// "" unless the operator configured cli.mcp_config; only ClaudeProtocol acts on it.
		MCPConfigFile: r.mcpConfigFile,
	}
}
