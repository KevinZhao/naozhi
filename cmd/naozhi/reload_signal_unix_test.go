//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// watchSignals registers SIGHUP: a SIGHUP sent to the process reaches reload
// and not shutdown. Unregistered, the signal's default action would end the
// test binary. Not parallel: it signals the whole process.
func TestWatchSignals_SIGHUPReloads(t *testing.T) {
	reloaded := make(chan struct{}, 1)
	stop := watchSignals(func() { reloaded <- struct{}{} }, func(reason string) {
		t.Errorf("shutdown(%q) on SIGHUP", reason)
	})
	t.Cleanup(stop)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not reach reload")
	}
}
