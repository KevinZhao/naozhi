package sysession

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// SweepOldJSONL deletes JSONL files in dir whose mtime is older than
// maxAge.  Returns the count of files removed and a non-nil error only
// if the directory itself can't be read; per-file delete errors are
// logged with slog.Warn and counted toward the success path (one bad
// file shouldn't abort the whole sweep).
//
// This is the Phase 1 gardening hook for dataDir/sys-sessions/ — every
// transient system session leaves a JSONL behind in the CLI's project
// dir, and at default 30s tick that's ~2880 files/day.  RFC §6.5 plans
// to upgrade this into a long-running TransientSweeper daemon in
// Phase 2; the function shape stays the same so the migration is a
// pull-up.
//
// We deliberately match only "*.jsonl" — claude CLI writes nothing
// else under cwd, but if a future binary version drops other file
// types (say, transient lock files) we'd rather not sweep them on
// behalf of behaviour we don't control.
func SweepOldJSONL(dir string, maxAge time.Duration) (int, error) {
	if dir == "" || maxAge <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing dir is fine — first run before any subprocess execs.
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("sysession: read sweep dir %q: %w", dir, err)
	}

	cutoff := time.Now().Add(-maxAge)
	deleted := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("sysession: stat sweep entry failed",
				"dir", dir, "entry", e.Name(), "err", err)
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err != nil {
			slog.Warn("sysession: remove sweep entry failed",
				"path", path, "err", err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		slog.Info("sysession: swept old JSONL",
			"dir", dir, "deleted", deleted, "max_age", maxAge)
	}
	return deleted, nil
}

// EnsureWorkDir creates dir with mode 0700 if it doesn't exist, or
// chmod's an existing dir to 0700.  Returns the absolute path.
//
// 0700 is load-bearing:  Runner subprocesses dump prompts (containing
// user conversation excerpts) into JSONL inside this dir; only the
// naozhi process user should be able to read them.
func EnsureWorkDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("sysession: resolve work dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("sysession: create work dir %q: %w", abs, err)
	}
	// MkdirAll does NOT chmod existing dirs — apply explicitly to
	// repair pre-existing 0755 leftovers from earlier naozhi versions.
	if err := os.Chmod(abs, 0o700); err != nil {
		return "", fmt.Errorf("sysession: chmod 0700 work dir %q: %w", abs, err)
	}
	return abs, nil
}
