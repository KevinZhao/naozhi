//go:build !windows

package project

import (
	"os"
	"syscall"
)

// fileOwnerUID returns the owning UID of a Lstat'd file. The bool reports
// whether the FileInfo carried a real *syscall.Stat_t — test fakes that
// return a stub Sys() get (0, false) and the caller treats that as
// "unable to determine ownership".
func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
