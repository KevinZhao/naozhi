package session

import "github.com/naozhi/naozhi/internal/cli"

// ArgvSpawnOptions builds the SpawnOptions subset that Protocol.BuildArgs turns
// into argv. THE one place it is built: the real spawn (reserveSpawn), the
// arg-drift comparison (driftCompareArgs) and `naozhi config check --effective`
// all come through here, so a field set on one path only cannot make them
// disagree. An argv-bearing field missing from the spawn side reads as permanent
// drift and kills every surviving shim on each naozhi restart; missing from the
// report side, it makes `config check` describe a spawn that never happens.
//
// The inputs are positional on purpose: a caller that forgets one does not
// compile. Add new argv-bearing fields HERE, not at a call site. debugFile is a
// parameter because spawn hardens the file and the other two paths stay read-only.
func ArgvSpawnOptions(model, effort, debugFile, systemPrompt string, extraArgs []string, settingsFile, mcpConfigFile string, permissionMode cli.PermissionMode) cli.SpawnOptions {
	return cli.SpawnOptions{
		Model:              model,
		Effort:             effort,
		ExtraArgs:          extraArgs,
		DebugFile:          debugFile,
		AppendSystemPrompt: systemPrompt,
		// "" unless the operator opted into naozhi-owned isolated settings
		// (RFC naozhi-owned-settings-v3); only ClaudeProtocol acts on it.
		SettingsFile: settingsFile,
		// "" unless the operator configured cli.mcp_config; only ClaudeProtocol acts on it.
		MCPConfigFile: mcpConfigFile,
		// cli.permission_mode; only ClaudeProtocol acts on it.
		PermissionMode: permissionMode,
	}
}

// argvSpawnOptions is ArgvSpawnOptions with the router-owned paths and
// permission mode filled in from the spawn facet.
func (c *spawnConfig) argvSpawnOptions(model, effort, debugFile, systemPrompt string, extraArgs []string) cli.SpawnOptions {
	return ArgvSpawnOptions(model, effort, debugFile, systemPrompt, extraArgs, c.naozhiSettingsFile, c.mcpConfigFile, c.permissionMode)
}
