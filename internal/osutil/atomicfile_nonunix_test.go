//go:build !unix

package osutil

// OBS2 regression test for the non-Unix IsDiskFull stub. Returning
// false on every input is the documented contract; this test just
// pins that contract so a future edit that tries to add partial
// Windows detection (e.g. via golang.org/x/sys/windows) without
// touching the Unix implementation will surface here as a
// visible test change.

import (
	"errors"
	"io/fs"
	"testing"
)

func TestIsDiskFull_NonUnixAlwaysFalse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain", errors.New("disk full")},
		{"path_error", &fs.PathError{Op: "write", Path: "/foo", Err: errors.New("ENOSPC")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsDiskFull(tc.err) {
				t.Errorf("IsDiskFull(%v) = true, want false on non-Unix", tc.err)
			}
		})
	}
}
