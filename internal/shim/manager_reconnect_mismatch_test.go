//go:build linux || darwin

package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestReconnect_BinaryMismatchDoesNotSignal pins that Reconnect drops the
// state file of a PID whose binary does not match but never signals it: the
// PID may belong to an unrelated process after PID reuse.
func TestReconnect_BinaryMismatchDoesNotSignal(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	m.naozhiBin = "/nonexistent/not-naozhi-binary"

	key := "test:reconnect-mismatch"
	keyHash := KeyHash(key)
	statePath := StateFilePath(dir, keyHash)
	if err := WriteStateFile(statePath, State{
		ShimPID:   os.Getpid(), // live PID, but "wrong binary"
		Socket:    SocketPath(keyHash),
		AuthToken: "dA==",
		Key:       key,
	}); err != nil {
		t.Fatal(err)
	}

	// We are the target PID: without a handler SIGUSR2 would kill the test
	// binary instead of failing an assertion.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	defer signal.Stop(sigCh)

	_, err := m.Reconnect(context.Background(), key, 0)
	if !errors.Is(err, ErrBinaryMismatch) {
		t.Fatalf("Reconnect err = %v, want one wrapping ErrBinaryMismatch", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should be removed on mismatch, stat err = %v", err)
	}
	select {
	case <-sigCh:
		t.Error("Reconnect sent SIGUSR2 to a PID whose binary did not match")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestSignalAfterFailedReconnect_SkipsBinaryMismatch pins the callers' SIGUSR2
// fallback: any other Reconnect failure still signals the PID, a wrapped
// ErrBinaryMismatch does not.
func TestSignalAfterFailedReconnect_SkipsBinaryMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"binary mismatch", fmt.Errorf("shim PID 1 %w", ErrBinaryMismatch), false},
		{"dial failure", errors.New("dial shim: connection refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan *os.ProcessState, 1)
			go func() {
				_ = cmd.Wait()
				exited <- cmd.ProcessState
			}()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-exited
			})

			if got := SignalAfterFailedReconnect(cmd.Process.Pid, tc.err); got != tc.want {
				t.Fatalf("SignalAfterFailedReconnect = %v, want %v", got, tc.want)
			}
			wait := 500 * time.Millisecond
			if tc.want {
				wait = 5 * time.Second
			}
			select {
			case ps := <-exited:
				exited <- ps // let Cleanup drain it
				ws, _ := ps.Sys().(syscall.WaitStatus)
				if !tc.want || ws.Signal() != syscall.SIGUSR2 {
					t.Errorf("sleeper exited (%v), want signalled=%v", ps, tc.want)
				}
			case <-time.After(wait):
				if tc.want {
					t.Error("sleeper still alive, want SIGUSR2 for a non-mismatch failure")
				}
			}
		})
	}
}
