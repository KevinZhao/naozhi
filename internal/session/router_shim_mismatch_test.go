//go:build linux || darwin

package session

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestShutdownShimViaReconnect_FallbackSkipsBinaryMismatch pins that the orphan
// path's SIGUSR2 fallback leaves a PID Reconnect rejected as a binary mismatch
// alone, while a Reconnect that fails for another reason still signals it. The
// "shim" is a sleep process, so its binary never matches the test binary.
func TestShutdownShimViaReconnect_FallbackSkipsBinaryMismatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		writeState bool // false: Reconnect fails reading the state file
		wantSignal bool
	}{
		{"binary mismatch", true, false},
		{"missing state file", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
			w.ShimManager = mgr

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

			key := "feishu:direct:alice:general"
			keyHash := shim.KeyHash(key)
			state := shim.State{
				ShimPID:   cmd.Process.Pid,
				Socket:    shim.SocketPath(keyHash),
				AuthToken: "dA==",
				Key:       key,
			}
			if tc.writeState {
				if err := shim.WriteStateFile(shim.StateFilePath(dir, keyHash), state); err != nil {
					t.Fatal(err)
				}
			}

			shutdownShimViaReconnect(context.Background(), w, state, 2*time.Second, true)
			if _, err := os.Stat(shim.StateFilePath(dir, keyHash)); !os.IsNotExist(err) {
				t.Fatalf("state file still present (stat err = %v): Reconnect did not run its rejection path", err)
			}

			wait := 500 * time.Millisecond
			if tc.wantSignal {
				wait = 5 * time.Second
			}
			select {
			case ps := <-exited:
				exited <- ps // let Cleanup drain it
				ws, _ := ps.Sys().(syscall.WaitStatus)
				if !tc.wantSignal || ws.Signal() != syscall.SIGUSR2 {
					t.Errorf("sleeper exited (%v), want signalled=%v", ps, tc.wantSignal)
				}
			case <-time.After(wait):
				if tc.wantSignal {
					t.Error("sleeper still alive, want the SIGUSR2 fallback")
				}
			}
		})
	}
}
