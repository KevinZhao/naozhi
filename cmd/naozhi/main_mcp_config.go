// Resolution + validation for the operator-owned MCP server definition file
// (`cli.mcp_config`, docs/rfc/cli-mcp-config.md). cc fails HARD on a missing /
// unparseable / mcpServers-less --mcp-config (no system/init, spawn dies), but
// degrades softly on an invalid individual server, so resolveMCPConfigFile
// validates only the envelope and returns "" on any failure. Server bodies stay
// unparsed json.RawMessage: they carry credentials and are never logged.
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

// mcpConfigEnvelope is the outer shape cc requires of an --mcp-config file;
// server definitions are deliberately left unparsed (see package doc).
type mcpConfigEnvelope struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// resolveMCPConfigFile returns the absolute path for SpawnOptions.MCPConfigFile,
// or "" to omit `--mcp-config`. Every rejection logs at Error and returns "";
// a permissive file mode only warns and still returns the path.
func resolveMCPConfigFile(cfg *config.Config) string {
	raw := cfg.CLI.MCPConfig
	if raw == "" {
		return ""
	}
	path := osutil.ExpandHome(raw)

	// Argv-injection guard: a leading '-' could be read as another CLI flag.
	if strings.HasPrefix(path, "-") {
		slog.Error("mcp config: path must not start with '-'; staying without mcp config",
			"path", path)
		return ""
	}
	// A relative path would resolve against each session's workspace cwd,
	// silently meaning a different file per session.
	if !filepath.IsAbs(path) {
		slog.Error("mcp config: path must be absolute; staying without mcp config",
			"path", path)
		return ""
	}

	// Stat before read: os.ReadFile on a FIFO / char device never returns and
	// would hang startup; a huge file would be read into memory for nothing.
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
	// A real envelope is a few KiB; 1 MiB bounds the read generously.
	const maxMCPConfigBytes = 1 << 20
	if fi.Size() > maxMCPConfigBytes {
		slog.Error("mcp config: file is implausibly large for an mcpServers envelope; staying without mcp config",
			"path", path, "size", fi.Size(), "max", maxMCPConfigBytes)
		return ""
	}

	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("mcp config: cannot read file; staying without mcp config",
			"path", path, "err", err)
		return ""
	}
	var env mcpConfigEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Deliberately does NOT log err: json error messages quote surrounding
		// bytes, which may include a bearer token from a headers block.
		slog.Error("mcp config: file is not a JSON object with an \"mcpServers\" object; staying without mcp config",
			"path", path)
		return ""
	}
	if env.MCPServers == nil {
		slog.Error("mcp config: file has no \"mcpServers\" object; staying without mcp config",
			"path", path)
		return ""
	}

	// Whoever can write this file gets arbitrary command execution in every
	// session via a stdio server. Warn, not fail: group-writable can be
	// legitimate in multi-operator deployments.
	if mode := fi.Mode().Perm(); mode&0o022 != 0 {
		slog.Warn("mcp config: file is group/other-writable; anyone who can write it can run arbitrary commands in every session (recommend chmod 600)",
			"path", path, "mode", mode.String())
	}

	slog.Info("mcp config: enabled", "path", path, "servers", len(env.MCPServers))
	return path
}
