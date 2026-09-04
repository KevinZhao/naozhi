//go:build !windows

package server

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// verifyProcOwnedByEuid is a defense-in-depth check that the process at pid
// runs under the same UID as naozhi itself, complementing the
// verifyProcIdentity PID/start_time guard.
//
// Linux-only: reads stat(2).Uid from /proc/<pid>. Returns nil on non-Linux
// Unix (no /proc) or when stat fails, so the caller falls back to the
// start_time check alone rather than blocking legitimate kills.
func verifyProcOwnedByEuid(pid int) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("refuse to kill PID %d: owner UID %d != euid %d", pid, st.Uid, os.Geteuid())
	}
	return nil
}
