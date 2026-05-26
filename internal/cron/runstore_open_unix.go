//go:build !windows

package cron

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openRunFile opens path for reading with strong symlink-traversal defense.
//
// O_NOFOLLOW: refuse to open a final-component symlink. If an attacker races
// a swap-to-symlink between our caller's path construction and Open, the
// open itself fails with ELOOP — a kernel-atomic guard, not a TOCTOU window.
// O_CLOEXEC: never let a forked child inherit the fd.
//
// ELOOP is mapped to ErrCorruptRun so callers can distinguish "missing"
// (fs.ErrNotExist) from "actively malicious" without leaking the syscall
// name to higher layers. Other errors (EACCES, EIO …) propagate unchanged.
func openRunFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: refused to follow symlink", ErrCorruptRun)
		}
		return nil, err
	}
	return f, nil
}
