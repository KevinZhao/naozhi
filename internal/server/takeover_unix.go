//go:build !windows

package server

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// verifyProcOwnedByEuid is a defense-in-depth check that the process at pid
// runs under the same UID as naozhi itself. Combined with verifyProcIdentity
// (PID/start_time TOCTOU guard) it eliminates the residual risk of a same-UID
// attacker constructing a process with a colliding (PID, start_time) under a
// matching cwd. R20260526-SEC-009.
//
// Linux-only: reads stat(2).Uid from /proc/<pid>. Returns nil on non-Linux
// Unix platforms (darwin has no /proc) or when stat fails (caller should rely
// on the start_time check alone in that case — we don't want to block
// legitimate kills on darwin where /proc isn't available).
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
