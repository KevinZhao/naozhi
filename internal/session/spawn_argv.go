package session

import "github.com/naozhi/naozhi/internal/cli"

// argvSpawnOptions builds the SpawnOptions subset that Protocol.BuildArgs turns
// into argv. It exists to keep the two callers bit-identical:
//
//   - the real spawn (spawnSession), which layers the non-argv fields (Key,
//     ResumeID, WorkingDir, timeouts) on top of the returned value, and
//   - the arg-drift comparison (driftCompareArgs), which reconstructs the argv a
//     fresh spawn WOULD use and restarts the session when it differs from the
//     argv its surviving shim recorded.
//
// Any field that lands in argv but is set on only one of those paths reads as
// permanent drift, so every naozhi restart shuts down the surviving shims and
// kills their CLIs. That has now happened four times — model/effort ("切过模型
// 的会话一重启全刷"), SettingsFile, MCPConfigFile, and DebugFile — each fixed by
// adding one more field to a hand-written struct literal and one more "must stay
// mirrored" comment. Routing both paths through this function converts the next
// occurrence from a silent zero-value into a compile error: the argv-bearing
// inputs are positional parameters, so a caller that forgets one does not build.
//
// debugFile is a parameter rather than a r.cliDebug*For(key) call inside the
// function because the two paths need different side effects for the same value:
// the spawn path pre-creates and hardens the log (cliDebugFileFor), while the
// drift path must stay read-only (cliDebugPathFor). See cli_debug.go.
//
// PermissionMode is deliberately absent: BuildArgs emits
// --dangerously-skip-permissions for the zero value, so both paths agree today.
// A caller that starts varying it must add it here, not at one call site.
func (r *Router) argvSpawnOptions(model, effort, debugFile string, extraArgs []string) cli.SpawnOptions {
	return cli.SpawnOptions{
		Model:     model,
		Effort:    effort,
		ExtraArgs: extraArgs,
		DebugFile: debugFile,
		// "" unless the operator opted into naozhi-owned isolated settings
		// (RFC naozhi-owned-settings-v3); only ClaudeProtocol acts on it.
		SettingsFile: r.naozhiSettingsFile,
		// "" unless the operator configured cli.mcp_config (RFC
		// cli-mcp-config); only ClaudeProtocol acts on it.
		MCPConfigFile: r.mcpConfigFile,
	}
}
