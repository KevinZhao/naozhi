//go:build !linux && !darwin

package shim

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// moveToShimsCgroup is a no-op: shim lifecycle separation is only implemented
// on Linux (cgroup/systemd scope) and Darwin (launchd reparenting).
func moveToShimsCgroup(_ context.Context, _, _ int, _ string) {}

// shimPIDBinaryMismatch returns an error on unsupported platforms; callers
// treat err != nil as "skip the gate" (PID-reuse detection disabled here).
func shimPIDBinaryMismatch(_ int, _ string) (bool, error) {
	return false, errors.New("binary identity check not implemented on this platform")
}

func ignoreHupPipe() {}
func notifyTerminate(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
func setUmask(_ int) int              { return 0 }
func notifyUSR2(_ chan<- os.Signal)   {}
func setSetsid(_ *exec.Cmd)           {}
func pidAlive(_ int) bool             { return false }
func sendSIGTERM(_ int) error         { return errors.New("signals not supported on this platform") }
func sendSIGUSR2(_ int) error         { return errors.New("signals not supported on this platform") }
func sendProcGroupSIGINT(_ int) error { return errors.New("signals not supported on this platform") }
func sendProcGroupSIGKILL(_ int) error {
	return errors.New("signals not supported on this platform")
}
