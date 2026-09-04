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

// openCronStoreFile opens the cron_jobs.json store path with the same
// O_NOFOLLOW + O_CLOEXEC guard as openRunFile (closes the Lstat→Open TOCTOU,
// #829). Errors are returned raw: loadJobs classifies ErrNotExist / ELOOP
// itself so the corrupt-rename branch can log a distinct "symlink swap" cause.
func openCronStoreFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

// isSymlinkLoopErr reports whether err is the kernel's "refused to follow
// symlink" signal from openCronStoreFile; the windows shim returns its own
// sentinel so callers stay platform-agnostic.
func isSymlinkLoopErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
