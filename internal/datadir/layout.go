// Package datadir centralises the on-disk layout policy for naozhi's data
// root: path constructors name where each subsystem's state lives under
// <dataDir>, and EnsureDir is the shared create-and-lock-down primitive
// (0o700, symlink/non-directory guard, perm tightening) so every adopter
// inherits the same hardening (#1175).
package datadir

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// DirMode is the mode for every naozhi-owned data directory: 0o700 keeps
// session state, event logs and cron JSON (script source, env values, output)
// unreadable by other OS users on a shared host.
const DirMode fs.FileMode = 0o700

// SessionsPath returns the session store file (<dataDir>/sessions.json);
// sidecars live in the same directory.
func SessionsPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "sessions.json")
}

// EventsRoot returns the per-session event-log directory (<dataDir>/events).
func EventsRoot(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "events")
}

// UISettingsPath returns the dashboard UI-preferences file
// (<dataDir>/ui-settings.json); one file for the whole single-user instance.
func UISettingsPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "ui-settings.json")
}

// CronJobsPath returns the cron job definitions file (<dataDir>/cron_jobs.json).
func CronJobsPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "cron_jobs.json")
}

// CronRunsRoot returns the cron run-record root (<dataDir>/runs). Per-job
// subdirectories live beneath it.
func CronRunsRoot(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "runs")
}

// CLIDebugRoot returns the per-session CLI debug-log directory
// (<dataDir>/cli-debug), populated only under NAOZHI_CLI_DEBUG. It holds raw
// `claude --debug-file` output (prompt/tool internals), hence 0o700 EnsureDir.
func CLIDebugRoot(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "cli-debug")
}

// EnsureDir creates path (and parents) at DirMode and tightens it: Lstat
// rejects a symlink or non-directory leaf (a planted <dataDir>/X → /etc
// symlink would redirect every write outside the data root; MkdirAll does not
// error on a symlink-to-dir) — this guard is the security boundary. A looser
// pre-existing mode is chmod'ed to 0o700; chmod failure is logged and
// tolerated (read-only / non-owned bind mounts). Empty path is a no-op.
func EnsureDir(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(path, DirMode); err != nil {
		return fmt.Errorf("create data directory %q: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat data directory %q: %w", path, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("data directory %q is a symlink or non-directory (mode %s); refusing to use", path, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != DirMode {
		if cerr := os.Chmod(path, DirMode); cerr != nil {
			slog.Warn("datadir: chmod to 0700 failed; leaving prior mode",
				"path", path, "had_mode", perm.String(), "err", cerr)
		} else {
			slog.Info("datadir: corrected directory mode to 0700",
				"path", path, "had_mode", perm.String())
		}
	}
	return nil
}
