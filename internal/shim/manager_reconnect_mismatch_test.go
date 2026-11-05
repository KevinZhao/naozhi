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

// TestBinaryMismatch_RetiresTheListeningShim: a shim started by a naozhi
// binary at another path cannot be reattached, so discovery and the pre-spawn
// step ask it to exit over its socket (its PID is never signalled) instead of
// only dropping its state file and leaving it bound, which refused the key's
// respawn. A shim that stays bound keeps its state file for the next try.
func TestBinaryMismatch_RetiresTheListeningShim(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spawn     bool // the pre-spawn step instead of Discover
		keepBound bool
	}{
		{"discover", false, false},
		{"pre-spawn", true, false},
		{"discover/still bound", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "feishu:direct:moved-binary:general"
			f := newRetireFake(t, key, boolPtr(true))
			f.keepBound = tc.keepBound
			f.serve()
			m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
			m.naozhiBin = "/nonexistent/old-path/naozhi"
			// The state's PID is this test process, so a signal would land here.
			writeRetireState(t, m, key, f, f.token, 0)
			sigCh := make(chan os.Signal, 2)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGUSR2)
			defer signal.Stop(sigCh)

			if tc.spawn {
				if err := m.prepareSocketForSpawn(context.Background(), key, f.path); err != nil {
					t.Fatalf("prepareSocketForSpawn = %v, want the socket freed", err)
				}
			} else {
				states, _, err := m.Discover()
				if err != nil || len(states) != 0 {
					t.Fatalf("Discover = (%v, %v), want no live shim", states, err)
				}
			}
			if bound := socketExists(f.path); bound != tc.keepBound {
				t.Errorf("socket present = %v, want %v", bound, tc.keepBound)
			}
			_, statErr := os.Stat(StateFilePath(m.stateDir, KeyHash(key)))
			if kept := statErr == nil; kept != tc.keepBound {
				t.Errorf("state file kept = %v, want %v", kept, tc.keepBound)
			}
			if msgs := f.messages(); len(msgs) != 1 || msgs[0].Type != "shutdown" {
				t.Errorf("fake received %+v, want exactly one shutdown", msgs)
			}
			select {
			case sig := <-sigCh:
				t.Errorf("the mismatched PID was sent %v", sig)
			default:
			}
		})
	}
}
