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

// openCronStoreFile opens the cron_jobs.json store path for reading with
// the same O_NOFOLLOW + O_CLOEXEC guard as openRunFile. Returned errors are
// raw — callers (loadJobs) classify ErrNotExist / ELOOP themselves because
// the corrupt-rename branch needs the syscall.ELOOP signal to log a clearly
// distinct "symlink swap" cause vs. the generic "open failed" path.
//
// R238-SEC-8 (#829): closes the Lstat→Open TOCTOU. Previously loadJobs
// ran os.Lstat to reject symlinks, then os.Open which is a separate syscall
// — a local attacker who can write the data dir could race a swap-to-symlink
// in the window between the two. OpenFile(O_NOFOLLOW) is kernel-atomic; the
// follow-up Fstat in loadJobs validates the resulting fd is a regular file.
func openCronStoreFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

// isSymlinkLoopErr reports whether err is the kernel's "refused to follow
// symlink" signal from openCronStoreFile. Wrapped here so the windows shim
// (which lacks O_NOFOLLOW) can return its own sentinel and callers stay
// platform-agnostic. R238-SEC-8.
func isSymlinkLoopErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
