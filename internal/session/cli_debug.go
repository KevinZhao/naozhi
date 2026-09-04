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
// debug capture, read once at NewRouter time. When truthy (envpolicy.EnvTruthy)
// each spawned CLI writes its `--debug-file` log under <dataDir>/cli-debug/.
// Unset keeps debug off: no flags added, no directory created.
const cliDebugEnvVar = "NAOZHI_CLI_DEBUG"

// resolveCLIDebugDir decides where (if anywhere) spawned CLIs should write
// their debug logs. It returns "" (debug disabled) unless the opt-in is clean:
// env unset/falsey → ""; eventLogDir empty (no data root to anchor under) → ""
// with an info log; directory creation fails → "" with a warning — a debug-dir
// problem must never block session spawning.
//
// The debug root is a sibling of the event-log dir under the same data root
// (<dataDir>/events and <dataDir>/cli-debug), derived from the event-log dir's
// parent. getenv is injected so tests can drive the env without os.Setenv races.
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
	// A relative --debug-file resolves against the subprocess CWD — the session
	// workspace — so a relatively-configured EventLogDir would land the debug
	// log (which may contain API keys) inside the workspace. Anchor to an
	// absolute path so the file is pinned regardless of spawn CWD (#2133).
	if !filepath.IsAbs(dataDir) {
		if abs, err := filepath.Abs(dataDir); err == nil {
			dataDir = abs
		} else {
			slog.Warn("cli debug dir could not be made absolute; debug capture disabled for this run",
				"dataDir", dataDir, "err", err)
			return ""
		}
	}
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

// cliDebugPathFor returns the per-session debug-file path WITHOUT touching the
// filesystem, or "" when CLI debug capture is off. The file name reuses the
// event-log key-hash stem so a session's debug log lines up with its <stem>.log
// event file. driftCompareArgs uses this (not cliDebugFileFor) so the startup
// drift pass never conjures debug logs for sessions that will not respawn.
func (r *Router) cliDebugPathFor(key string) string {
	if r.cliDebugDir == "" {
		return ""
	}
	return filepath.Join(r.cliDebugDir, persist.KeyHash(key)+".log")
}

// cliDebugFileFor returns cliDebugPathFor's path after pre-creating and
// hardening the file, for the spawn path. The file is overwritten on every
// spawn — debug capture is a live-tail diagnostic, not an audit trail.
func (r *Router) cliDebugFileFor(key string) string {
	path := r.cliDebugPathFor(key)
	if path == "" {
		return ""
	}
	// The claude child creates --debug-file under its own umask, so the log
	// (which may contain API keys) can land world-readable; pre-create at 0600
	// and Chmod to repair a pre-existing file O_CREATE leaves untouched (#2171).
	// No O_EXCL — the file legitimately pre-exists from a prior spawn. Errors
	// are fail-open (warn + still return path): hardening must never block a spawn.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		slog.Warn("cli debug file pre-create failed; continuing without hardening",
			"path", path, "err", err)
	} else {
		_ = f.Close()
		if err := os.Chmod(path, 0o600); err != nil {
			slog.Warn("cli debug file chmod 0600 failed; log may be world-readable",
				"path", path, "err", err)
		}
	}
	return path
}
