//go:build unix

package osutil

// OBS2 regression tests for the Unix IsDiskFull implementation. These
// pin both the happy path (raw + wrapped + arbitrarily-nested errors
// containing syscall.ENOSPC all report disk-full) and the negative
// cases (other errnos, nil, string-only errors do not false-positive).
// Splitting the file so the tests carry the same //go:build unix tag
// as the implementation — otherwise they'd fail to build on Windows
// because syscall.ENOSPC has different semantics there.

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

func TestIsDiskFull_RawENOSPC(t *testing.T) {
	t.Parallel()
	if !IsDiskFull(syscall.ENOSPC) {
		t.Error("IsDiskFull(ENOSPC) = false, want true")
	}
}

func TestIsDiskFull_WrappedENOSPC(t *testing.T) {
	t.Parallel()
	// Typical shape emitted by os.Write / f.Sync on a full disk.
	wrapped := &fs.PathError{Op: "write", Path: "/foo", Err: syscall.ENOSPC}
	if !IsDiskFull(wrapped) {
		t.Error("IsDiskFull(wrapped PathError) = false, want true")
	}
}

func TestIsDiskFull_DeeplyNestedENOSPC(t *testing.T) {
	t.Parallel()
	base := &fs.PathError{Op: "rename", Path: "/a", Err: syscall.ENOSPC}
	outer := fmt.Errorf("save cron store: %w", fmt.Errorf("write-tmp: %w", base))
	if !IsDiskFull(outer) {
		t.Error("IsDiskFull(deeply wrapped ENOSPC) = false, want true")
	}
}

func TestIsDiskFull_OtherErrno(t *testing.T) {
	t.Parallel()
	// Permission denied must not be misclassified as disk-full.
	if IsDiskFull(syscall.EACCES) {
		t.Error("IsDiskFull(EACCES) = true, want false")
	}
	// EINVAL (used by SyncDir's soft-failure path) must not trip either.
	if IsDiskFull(syscall.EINVAL) {
		t.Error("IsDiskFull(EINVAL) = true, want false")
	}
}

func TestIsDiskFull_Nil(t *testing.T) {
	t.Parallel()
	if IsDiskFull(nil) {
		t.Error("IsDiskFull(nil) = true, want false")
	}
}

func TestIsDiskFull_StringOnlyError(t *testing.T) {
	t.Parallel()
	// An errors.New string with "no space left on device" in its message
	// but no underlying syscall.ENOSPC must not match — IsDiskFull relies
	// on errors.Is (sentinel identity) not message matching.
	plain := errors.New("write failed: no space left on device")
	if IsDiskFull(plain) {
		t.Error("IsDiskFull(plain string) = true, want false (must not string-match)")
	}
}
