//go:build linux || darwin

package shim

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func ignoreHupPipe() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
}

// notifyTerminate registers ch to receive SIGTERM and SIGINT.
func notifyTerminate(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
}

// setUmask sets the file creation mask and returns the previous value.
func setUmask(mask int) int {
	return syscall.Umask(mask)
}

// notifyUSR2 registers ch to receive SIGUSR2.
func notifyUSR2(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGUSR2)
}

// setSetsid sets Setsid on cmd so it starts in a new session, detached from
// the parent process group. This allows the shim to outlive its parent.
func setSetsid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// pidAlive returns true if the process exists (kill -0 succeeds).
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// sendSIGTERM sends SIGTERM to a single process.
func sendSIGTERM(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// sendSIGUSR2 sends SIGUSR2 to a single process.
func sendSIGUSR2(pid int) error {
	return syscall.Kill(pid, syscall.SIGUSR2)
}

// sendProcGroupSIGINT sends SIGINT to the entire process group of pid.
func sendProcGroupSIGINT(pid int) error {
	return syscall.Kill(-pid, syscall.SIGINT)
}

// sendProcGroupSIGKILL sends SIGKILL to the entire process group of pid.
func sendProcGroupSIGKILL(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
