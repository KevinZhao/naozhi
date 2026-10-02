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
	dir, why := cliDebugDirWith(eventLogDir, getenv)
	if dir == "" {
		if why != "" {
			slog.Info(why, "env", cliDebugEnvVar, "event_log_dir", eventLogDir)
		}
		return ""
	}
	if err := datadir.EnsureDir(dir); err != nil {
		slog.Warn("cli debug dir unusable; debug capture disabled for this run",
			"dir", dir, "err", err)
		return ""
	}
	slog.Info("cli debug capture enabled; spawned CLIs will write --debug-file logs",
		"dir", dir)
	return dir
}

// CLIDebugDir is the debug-log root for eventLogDir, or "" when capture is off
// (env opt-in unset, no data root to anchor under, or an unresolvable relative
// path). Pure: no directory creation, no logging — `naozhi config check` uses it
// to report the --debug-file a spawn would pass without creating anything.
func CLIDebugDir(eventLogDir string) string {
	dir, _ := cliDebugDirWith(eventLogDir, os.Getenv)
	return dir
}

// cliDebugDirWith computes the root; why explains an empty result for the
// caller that logs (empty why = capture simply not requested).
func cliDebugDirWith(eventLogDir string, getenv func(string) string) (dir, why string) {
	if !envpolicy.EnvTruthy(getenv(cliDebugEnvVar)) {
		return "", ""
	}
	if eventLogDir == "" {
		return "", "cli debug capture requested but event log is disabled; no data root to anchor under — debug capture stays off"
	}
	// The root is the PARENT of the configured event-log dir, not the session
	// store's directory: cli-debug is gated on the event log being enabled and
	// has always anchored to it, so an operator who moved event_log_dir keeps
	// both together. That coupling is worth revisiting (#2544) but changing it
	// here would relocate an existing operator's debug logs.
	dataDir := filepath.Dir(eventLogDir)
	// A relative --debug-file resolves against the subprocess CWD — the session
	// workspace — so a relatively-configured EventLogDir would land the debug
	// log (which may contain API keys) inside the workspace. Anchor to an
	// absolute path so the file is pinned regardless of spawn CWD (#2133).
	if !filepath.IsAbs(dataDir) {
		if abs, err := filepath.Abs(dataDir); err == nil {
			dataDir = abs
		} else {
			return "", "cli debug dir could not be made absolute; debug capture disabled for this run"
		}
	}
	return datadir.FromRoot(dataDir).CLIDebugRoot(), ""
}

// CLIDebugPath returns the debug-log path a session key gets under dir, or ""
// when debug capture is off (dir empty). Exported so `naozhi config check
// --effective` reports the same `--debug-file` value a spawn would pass instead
// of re-deriving the name.
func CLIDebugPath(dir, key string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, persist.KeyHash(key)+".log")
}
