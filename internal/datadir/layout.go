// Package datadir centralises the on-disk layout policy for naozhi's data
// root. Before this package each subsystem (session store, event log, cron
// jobs + runs) independently joined its own paths under <dataDir>/ and ran
// its own os.MkdirAll(..., 0o700) with subtly different hardening, so adding
// a fourth subsystem duplicated the dance again and there was no single
// source of truth for "given the data root, where does X live and how is its
// directory mode/symlink policy enforced". R250-ARCH-13 (#1175).
//
// Two halves:
//   - Path constructors (SessionsPath / EventsRoot / CronJobsPath /
//     CronRunsRoot) name the canonical location of each subsystem's state
//     relative to a shared <dataDir>.
//   - EnsureDir is the shared "create + lock down" primitive: MkdirAll at
//     0o700, then a symlink / non-directory guard and a perm-tightening
//     chmod so a pre-existing 0o755 tree (laid down by an older version or
//     raced ahead by another local user) is corrected, and a redirect via a
//     planted symlink is refused. This mirrors the hardening the cron run
//     store grew (R234-SEC-4 / R245-SEC-1 / R247-SEC-12) so new adopters
//     inherit it for free instead of re-deriving a weaker version.
package datadir

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// DirMode is the contractual mode for every naozhi-owned data directory.
// 0o700 keeps session state, event logs, and cron job/run JSON (which embed
// script source, env values, and output summaries) unreadable by other OS
// users on a shared host.
const DirMode fs.FileMode = 0o700

// SessionsPath returns the session store file (<dataDir>/sessions.json).
// Sidecars (meta, known-ids, workspace-overrides) are derived from this path
// by the session package and live in the same directory.
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

// CronJobsPath returns the cron job definitions file
// (<dataDir>/cron_jobs.json).
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

// EnsureDir creates path (and parents) at DirMode and tightens it down to a
// safe state, returning an error only when path cannot be made usable as a
// private directory.
//
// Steps:
//  1. MkdirAll(path, 0o700) — fails hard if the directory can't be created.
//  2. Lstat the leaf: reject a symlink or non-directory (a planted
//     <dataDir>/X → /etc symlink would otherwise silently redirect every
//     subsequent write outside the data root; MkdirAll does not error on a
//     symlink-to-dir). This is the authoritative redirect guard.
//  3. Chmod the leaf to 0o700 when it carries looser perms. MkdirAll only
//     applies perm to directories it actually creates, so a pre-existing
//     0o755 tree keeps its mode without this step. Chmod failure is logged
//     and tolerated (containers with read-only / non-owned bind mounts can't
//     chmod) — the Lstat redirect guard, not the mode, is the security
//     boundary.
//
// Empty path is a no-op (nil) so callers that derive the path from an
// unset data root degrade quietly, matching the prior os.MkdirAll-guarded
// call sites.
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
