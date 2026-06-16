//go:build !windows

package project

import "syscall"

// mkfifoForTest creates a FIFO so the listing handler's irregular-type skip
// can be exercised. Unix-only; the test that calls it skips on windows.
func mkfifoForTest(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
