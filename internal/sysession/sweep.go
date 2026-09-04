package sysession

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// SweepOldJSONL performs ONE pass over dir (no internal ticker; callers
// schedule it) deleting "*.jsonl" files whose mtime is older than maxAge.
// Returns the count removed; the error is non-nil only if the directory
// itself can't be read — per-file delete errors are logged and skipped. Only
// .jsonl is matched so files a future CLI might drop (e.g. lock files) are
// never swept on behalf of behaviour we don't control. Gardening hook for
// dataDir/sys-sessions/, where every transient system session leaves a JSONL
// (~2880/day at a 30s tick).
func SweepOldJSONL(dir string, maxAge time.Duration) (int, error) {
	if dir == "" || maxAge <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing dir is fine — first run before any subprocess execs.
		if errors.Is(err, fs.ErrNotExist) {
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

// EnsureWorkDir creates dir with mode 0700 (or chmods an existing dir to
// 0700) and returns the absolute path. 0700 is load-bearing: Runner
// subprocesses dump prompts containing user conversation excerpts into JSONL
// here; only the naozhi process user may read them.
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
