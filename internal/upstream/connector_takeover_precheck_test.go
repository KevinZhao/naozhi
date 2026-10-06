package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestHandleRequest_Takeover_RefusalKeepsCLIAlive pins #3315 on the
// reverse-connected path: a takeover the node refuses, by its router or by
// the cwd check, is answered before SIGTERM, so the external CLI survives.
func TestHandleRequest_Takeover_RefusalKeepsCLIAlive(t *testing.T) {
	cases := []struct {
		name    string
		cwd     string
		stop    bool
		wantErr error
		wantMsg string
	}{
		{name: "router stopped", stop: true, wantErr: session.ErrRouterStopped},
		{name: "invalid cwd", cwd: "relative/dir", wantMsg: "takeover cwd invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Skipf("cannot start child: %v", err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			start, err := discovery.ProcStartTime(cmd.Process.Pid)
			if err != nil || start == 0 {
				t.Skipf("no process start time on this platform: %v", err)
			}

			r := makeRouter()
			if tc.stop {
				r.Shutdown()
			}
			c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
			params, _ := json.Marshal(map[string]any{
				"pid": cmd.Process.Pid, "session_id": "12345678-1234-1234-1234-123456789012",
				"cwd": tc.cwd, "proc_start_time": start,
			})
			var wg sync.WaitGroup
			_, err = c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "takeover", Params: params}, &wg)
			wg.Wait()
			if err == nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("takeover err = %v, want %v / %q", err, tc.wantErr, tc.wantMsg)
			}

			// A zombie still passes kill(pid, 0), so read how the child died:
			// by this SIGKILL, not by a SIGTERM from the connector.
			_ = cmd.Process.Kill()
			st, err := cmd.Process.Wait()
			if err != nil {
				t.Fatalf("reap child: %v", err)
			}
			if ws, ok := st.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("refused takeover still killed the external CLI: %v", st)
			}
		})
	}
}

func takeoverParams(t *testing.T, pid int, sessionID string, start uint64) json.RawMessage {
	t.Helper()
	params, err := json.Marshal(map[string]any{"pid": pid, "session_id": sessionID, "proc_start_time": start})
	if err != nil {
		t.Fatal(err)
	}
	return params
}

// TestHandleRequest_Takeover_SecondTakeoverOfKeyKeepsItsCLI pins #3417 on the
// reverse-connected path: the first takeover holds its key from before its
// SIGTERM until the spawn, so a second one of the same key, arriving while
// the first CLI is still exiting, is refused and its CLI never signalled.
func TestHandleRequest_Takeover_SecondTakeoverOfKeyKeepsItsCLI(t *testing.T) {
	dir := t.TempDir()
	termed, trapped := filepath.Join(dir, "termed"), filepath.Join(dir, "trapped")
	// Survives SIGTERM, recording it, so the exit wait lasts until appCtx ends.
	cmd1 := exec.Command("sh", "-c", `trap 'echo > "$0"' TERM; echo > "$1"; while :; do sleep 0.05; done`, termed, trapped)
	if err := cmd1.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	exited1 := make(chan struct{})
	go func() { _ = cmd1.Wait(); close(exited1) }()
	t.Cleanup(func() { _ = cmd1.Process.Kill(); <-exited1 })
	testhelper.Eventually(t, func() bool {
		_, err := os.Stat(trapped)
		return err == nil
	}, 5*time.Second, "the child never installed its SIGTERM trap")
	cmd2 := exec.Command("sleep", "30")
	if err := cmd2.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd2.Process.Kill(); _, _ = cmd2.Process.Wait() })
	start1, err1 := discovery.ProcStartTime(cmd1.Process.Pid)
	start2, err2 := discovery.ProcStartTime(cmd2.Process.Pid)
	if err1 != nil || err2 != nil || start1 == 0 || start2 == 0 {
		t.Skipf("no process start time on this platform: %v %v", err1, err2)
	}

	r := makeRouter()
	t.Cleanup(r.Shutdown)
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
	c.claudeDir = t.TempDir()
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()

	// No cwd: both map to the "unknown" cwd's key.
	first := node.ReverseMsg{Method: "takeover", Params: takeoverParams(t, cmd1.Process.Pid, "12345678-1234-1234-1234-123456789012", start1)}
	firstAt := time.Now()
	if _, err := c.handleRequest(appCtx, context.Background(), first, &wg); err != nil {
		t.Fatalf("first takeover: %v", err)
	}
	testhelper.Eventually(t, func() bool {
		_, err := os.Stat(termed)
		return err == nil
	}, 5*time.Second, "the first takeover's CLI never got its SIGTERM")
	second := node.ReverseMsg{Method: "takeover", Params: takeoverParams(t, cmd2.Process.Pid, "12345678-1234-1234-1234-123456789013", start2)}
	if _, err := c.handleRequest(appCtx, context.Background(), second, &wg); !errors.Is(err, session.ErrSpawnInFlight) {
		t.Fatalf("second takeover during the exit wait = %v, want ErrSpawnInFlight", err)
	}
	_ = cmd2.Process.Kill()
	st, err := cmd2.Process.Wait()
	if err != nil {
		t.Fatalf("reap child: %v", err)
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the refused takeover's CLI died of %v, not the test's SIGKILL", st)
	}
	// waitForExit SIGKILLs the trapping child after 5s, so only a run well
	// inside that deadline can tell an early exit apart.
	select {
	case <-exited1:
		if time.Since(firstAt) < 4*time.Second {
			t.Fatal("the first takeover's CLI exited before the exit wait ended")
		}
	default:
	}

	// Ending appCtx ends the exit wait; the goroutine then gives the key back.
	cancel()
	wg.Wait()
	lease, err := r.ReserveTakeover(sessionkey.TakeoverKey(sessionkey.SanitizeCWDKey("unknown")), sessionview.AgentOpts{})
	if err != nil {
		t.Fatalf("ReserveTakeover after the takeover ended: %v", err)
	}
	lease.Release()
}

// TestHandleRequest_Takeover_RefusalAfterReserveReleasesTheKey: a takeover
// refused after it reserved the key, at the identity check or the SIGTERM,
// gives the reservation back, or the key would stay refused until restart.
func TestHandleRequest_Takeover_RefusalAfterReserveReleasesTheKey(t *testing.T) {
	cases := []struct {
		name    string
		target  func(t *testing.T) (pid int, start uint64)
		wantMsg string
	}{
		{
			name: "identity unreadable",
			target: func(t *testing.T) (int, uint64) {
				cmd := exec.Command("true")
				if err := cmd.Run(); err != nil {
					t.Skipf("cannot run child: %v", err)
				}
				return cmd.Process.Pid, 1 // reaped, so its start time is gone
			},
			wantMsg: "cannot verify process identity",
		},
		{
			name: "pid reused",
			target: func(t *testing.T) (int, uint64) {
				cmd := exec.Command("sleep", "30")
				if err := cmd.Start(); err != nil {
					t.Skipf("cannot start child: %v", err)
				}
				t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
				start, err := discovery.ProcStartTime(cmd.Process.Pid)
				if err != nil || start == 0 {
					t.Skipf("no process start time on this platform: %v", err)
				}
				return cmd.Process.Pid, start + 1
			},
			wantMsg: "process identity mismatch",
		},
		{
			name: "sigterm refused",
			target: func(t *testing.T) (int, uint64) {
				// PID 1 is the target only where signalling it is refused, so
				// the test never delivers a signal.
				initProc, err := os.FindProcess(1)
				if err != nil || !errors.Is(initProc.Signal(syscall.Signal(0)), syscall.EPERM) {
					t.Skip("needs a PID 1 this user may not signal")
				}
				start, err := discovery.ProcStartTime(1)
				if err != nil || start == 0 {
					t.Skipf("no start time for PID 1: %v", err)
				}
				return 1, start
			},
			wantMsg: "kill process 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid, start := tc.target(t)
			r := makeRouter()
			t.Cleanup(r.Shutdown)
			c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(r), nil, nil, Discovery{}, nil)
			c.claudeDir = t.TempDir()
			var wg sync.WaitGroup
			req := node.ReverseMsg{Method: "takeover", Params: takeoverParams(t, pid, "12345678-1234-1234-1234-123456789012", start)}
			_, err := c.handleRequest(context.Background(), context.Background(), req, &wg)
			wg.Wait()
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("takeover err = %v, want %q", err, tc.wantMsg)
			}
			lease, err := r.ReserveTakeover(sessionkey.TakeoverKey(sessionkey.SanitizeCWDKey("unknown")), sessionview.AgentOpts{})
			if err != nil {
				t.Fatalf("ReserveTakeover after the refusal: %v", err)
			}
			lease.Release()
		})
	}
}
