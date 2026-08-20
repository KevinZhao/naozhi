// File: main_mcp_config.go
//
// Resolution + validation for the operator-owned MCP server definition file
// (`cli.mcp_config`, RFC docs/rfc/cli-mcp-config.md).
//
// Why this needs its own validation pass rather than just being handed to the
// CLI: measured on cc 2.1.235, `--mcp-config` fails CLOSED and HARD. If the
// file is missing, unparseable, or lacks an `mcpServers` object, cc prints
// `Error: Invalid MCP configuration: ...` and exits WITHOUT emitting a
// system/init event — i.e. the spawn fails outright. Handing an unvalidated
// path to the router would let a single stray comma in the MCP file escalate
// from "MCP unavailable" to "every naozhi session fails to start".
//
// So resolveMCPConfigFile validates the envelope up front and returns "" on any
// failure, degrading to "no MCP" (RFC G5). The failure boundary was measured,
// not guessed:
//
//	missing file / bad JSON / no mcpServers key  → cc REFUSES TO START (hard)
//	mcpServers present, one server def invalid   → cc starts fine, reports it
//	                                               in system/init's
//	                                               mcp_server_errors[] (soft)
//
// That boundary is why validation stops at the envelope: per-server problems
// are cc's to report and degrade over, so this file never inspects a server
// definition. It also keeps naozhi from parsing the parts that carry secrets —
// a remote server's `headers` block (bearer tokens) stays an unparsed
// json.RawMessage here, is never retained, and is never logged.
package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
)

// mcpConfigEnvelope is the outer shape cc requires of an --mcp-config file.
// Server definitions stay json.RawMessage on purpose: cc degrades gracefully on
// a malformed individual server (reporting it via system/init's
// mcp_server_errors), so validating them here would reject configs cc accepts —
// and would mean parsing the fields that carry credentials.
type mcpConfigEnvelope struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// resolveMCPConfigFile returns the absolute path the router should hand to every
// ClaudeProtocol spawn via SpawnOptions.MCPConfigFile, or "" to omit
// `--mcp-config` entirely.
//
// Every rejection path returns "" and logs at Error, never a path that would
// make cc fail to start. The one non-fatal finding is a permissive file mode,
// which logs at Warn and still returns the path (see below).
func resolveMCPConfigFile(cfg *config.Config) string {
	raw := cfg.CLI.MCPConfig
	if raw == "" {
		return ""
	}
	path := osutil.ExpandHome(raw)

	// Argv-injection guard, mirroring resolveNaozhiSettingsFile / the
	// --debug-file validator: a leading '-' could be reinterpreted by the CLI
	// as another flag. Checked before IsAbs so the log names the real problem.
	if strings.HasPrefix(path, "-") {
		slog.Error("mcp config: path must not start with '-'; staying without mcp config",
			"path", path)
		return ""
	}
	// Must be absolute: BuildArgs drops a relative value (defence in depth), and
	// a relative path would otherwise resolve against the CLI subprocess's cwd
	// (the session workspace), silently meaning a different file per session.
	if !filepath.IsAbs(path) {
		slog.Error("mcp config: path must be absolute; staying without mcp config",
			"path", path)
		return ""
	}

	// Stat before read. Two reasons, both availability (diff review D1):
	//   - os.ReadFile on a FIFO / character device never returns, so a config
	//     typo pointing at one would hang startup forever — a worse outcome
	//     than the hard spawn failure G5 exists to prevent, and one that
	//     happens BEFORE any of the validation below gets a chance to run.
	//   - a multi-GB path would be read fully into memory for nothing.
	// The same FileInfo feeds the permission check further down, so this costs
	// no extra syscall over the previous shape.
	fi, err := os.Stat(path)
	if err != nil {
		slog.Error("mcp config: cannot stat file; staying without mcp config",
			"path", path, "err", err)
		return ""
	}
	if !fi.Mode().IsRegular() {
		slog.Error("mcp config: path is not a regular file; staying without mcp config",
			"path", path, "mode", fi.Mode().String())
		return ""
	}
	// A legitimate mcpServers envelope is a few KiB; 1 MiB is generous enough
	// that no real config trips it while still bounding the read.
	const maxMCPConfigBytes = 1 << 20
	if fi.Size() > maxMCPConfigBytes {
		slog.Error("mcp config: file is implausibly large for an mcpServers envelope; staying without mcp config",
			"path", path, "size", fi.Size(), "max", maxMCPConfigBytes)
		return ""
	}

	// Envelope validation (RFC G5). Read once, here, at startup — not per spawn.
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("mcp config: cannot read file; staying without mcp config",
			"path", path, "err", err)
		return ""
	}
	var env mcpConfigEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Covers both "not JSON at all" and "valid JSON of the wrong shape"
		// (e.g. `{"mcpServers": []}`), hence the shape-neutral wording.
		//
		// Deliberately does NOT log err: a json syntax/type error message
		// quotes surrounding bytes, which may include a bearer token from a
		// remote server's headers block.
		slog.Error("mcp config: file is not a JSON object with an \"mcpServers\" object; staying without mcp config",
			"path", path)
		return ""
	}
	if env.MCPServers == nil {
		slog.Error("mcp config: file has no \"mcpServers\" object; staying without mcp config",
			"path", path)
		return ""
	}

	// Permissive-mode warning (not a rejection). Whoever can write this file can
	// give every cc session a stdio MCP server, i.e. arbitrary command
	// execution. Warn rather than fail: a group-writable path can be legitimate
	// in a multi-operator deployment, and turning a mode check into a startup
	// failure would trade a security nudge for an availability regression.
	if mode := fi.Mode().Perm(); mode&0o022 != 0 {
		slog.Warn("mcp config: file is group/other-writable; anyone who can write it can run arbitrary commands in every session (recommend chmod 600)",
			"path", path, "mode", mode.String())
	}

	slog.Info("mcp config: enabled", "path", path, "servers", len(env.MCPServers))
	return path
}
