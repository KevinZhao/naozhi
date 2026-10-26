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

func TestIsDiskFull(t *testing.T) {
	t.Parallel()
	base := &fs.PathError{Op: "rename", Path: "/a", Err: syscall.ENOSPC}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"raw ENOSPC", syscall.ENOSPC, true},
		// Typical shape emitted by os.Write / f.Sync on a full disk.
		{"wrapped PathError", &fs.PathError{Op: "write", Path: "/foo", Err: syscall.ENOSPC}, true},
		{"deeply wrapped", fmt.Errorf("save cron store: %w", fmt.Errorf("write-tmp: %w", base)), true},
		// Permission denied must not be misclassified as disk-full.
		{"EACCES", syscall.EACCES, false},
		// EINVAL (used by SyncDir's soft-failure path) must not trip either.
		{"EINVAL", syscall.EINVAL, false},
		{"nil", nil, false},
		// IsDiskFull relies on errors.Is (sentinel identity), not message matching.
		{"string-only", errors.New("write failed: no space left on device"), false},
	}
	for _, tc := range cases {
		if got := IsDiskFull(tc.err); got != tc.want {
			t.Errorf("IsDiskFull(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
