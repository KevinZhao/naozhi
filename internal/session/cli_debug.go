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
	// SEC-8 (#2133): the debug-file path is handed to the CLI subprocess as
	// --debug-file. A relative path resolves against the subprocess CWD —
	// the session workspace — so a relatively-configured EventLogDir would
	// land the debug log (which may contain API keys) inside the session
	// workspace. Anchor the debug root to an absolute path so the file is
	// pinned to a stable location regardless of where the CLI is spawned.
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
	path := filepath.Join(r.cliDebugDir, persist.KeyHash(key)+".log")
	// SEC (#2171): the spawned claude child creates --debug-file under its own
	// umask, so the log (which may contain API keys) can land 0644 / world-
	// readable even though the parent dir is 0700. Pre-create and harden to
	// 0600 here. No O_EXCL — the file legitimately pre-exists from a prior
	// spawn (capture overwrites on each run). The follow-up Chmod repairs a
	// pre-existing world-readable file that O_CREATE leaves untouched. All
	// errors are fail-open (warn + still return path): debug hardening must
	// never block a session spawn, matching resolveCLIDebugDirWith's posture.
	// 0600 open mirrors internal/cron/sandbox.go (without O_EXCL).
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
