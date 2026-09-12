package sysession

import (
	"fmt"
	"os"
	"path/filepath"
)

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
