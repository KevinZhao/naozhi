package session

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// cliDebugEnvVar is the operator opt-in switch for per-session Claude CLI
// debug capture. It is read once at NewRouter time. When truthy (any
// non-empty, non-"0"/"false"/"off" value — see envpolicy.EnvTruthy) the
// router asks each spawned CLI to write its `--debug-file` log under
// <dataDir>/cli-debug/. Default (unset) keeps debug off: no flags are added
// and no directory is created, so other deployments are unaffected.
const cliDebugEnvVar = "NAOZHI_CLI_DEBUG"

// resolveCLIDebugDir decides where (if anywhere) spawned CLIs should write
// their debug logs. It returns "" — debug disabled — in every case except a
// clean opt-in:
//
//   - NAOZHI_CLI_DEBUG unset / falsey → "".
//   - eventLogDir empty (event log disabled → no data root to anchor under)
//     → "" with an info log so the operator knows why the opt-in no-op'd.
//   - directory creation/hardening fails → "" with a warning; a debug-dir
//     problem must never block session spawning.
//
// The CLI debug root is a sibling of the event-log dir under the same data
// root (<dataDir>/events and <dataDir>/cli-debug), so it is derived from the
// event-log dir's parent rather than threading a separate dataDir field
// through RouterConfig.
//
// getenv is injected so tests can drive the env without os.Setenv races; the
// production caller passes os.Getenv.
func resolveCLIDebugDir(eventLogDir string) string {
	return resolveCLIDebugDirWith(eventLogDir, os.Getenv)
}

func resolveCLIDebugDirWith(eventLogDir string, getenv func(string) string) string {
	if !envpolicy.EnvTruthy(getenv(cliDebugEnvVar)) {
		return ""
	}
	if eventLogDir == "" {
		slog.Info("cli debug capture requested but event log is disabled; no data root to anchor under — debug capture stays off",
			"env", cliDebugEnvVar)
		return ""
	}
	dataDir := filepath.Dir(eventLogDir)
	dir := datadir.CLIDebugRoot(dataDir)
	if err := datadir.EnsureDir(dir); err != nil {
		slog.Warn("cli debug dir unusable; debug capture disabled for this run",
			"dir", dir, "err", err)
		return ""
	}
	slog.Info("cli debug capture enabled; spawned CLIs will write --debug-file logs",
		"dir", dir)
	return dir
}

// cliDebugFileFor returns the per-session debug-file path, or "" when CLI
// debug capture is off (cliDebugDir empty). The file name reuses the
// event-log key-hash stem so operators can line a session's debug log up with
// its <stem>.log event file. The path is regenerated (overwritten) on every
// spawn — debug capture is a live-tail diagnostic, not an audit trail, so the
// latest spawn's log is the one that matters.
func (r *Router) cliDebugFileFor(key string) string {
	if r.cliDebugDir == "" {
		return ""
	}
	return filepath.Join(r.cliDebugDir, persist.KeyHash(key)+".log")
}
