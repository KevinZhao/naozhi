//go:build !windows

package cron

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openRunFile opens path for reading with O_NOFOLLOW (a final-component
// symlink fails with ELOOP — kernel-atomic, no TOCTOU window) and O_CLOEXEC.
// ELOOP is mapped to ErrCorruptRun so callers can distinguish "missing" from
// "actively malicious"; other errors propagate unchanged.
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
