package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestVerifyProcOwnedByEuid_Self confirms that the helper accepts a process
// that runs under the current effective UID (the test process itself).
// R20260526-SEC-009.
func TestVerifyProcOwnedByEuid_Self(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UID check is Linux-only (uses /proc)")
	}
	if err := verifyProcOwnedByEuid(os.Getpid()); err != nil {
		t.Errorf("verifyProcOwnedByEuid(self) = %v, want nil", err)
	}
}

// TestVerifyProcOwnedByEuid_Init confirms the helper rejects PID 1 when the
// test runs as a non-root user (PID 1 is owned by root). When the test runs
// as root (e.g. inside some CI containers), euid==0 matches and we skip.
// R20260526-SEC-009.
func TestVerifyProcOwnedByEuid_Init(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UID check is Linux-only (uses /proc)")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: euid matches PID 1's UID, can't exercise mismatch path")
	}
	if _, err := os.Stat("/proc/1"); err != nil {
		t.Skipf("/proc/1 unavailable: %v", err)
	}
	err := verifyProcOwnedByEuid(1)
	if err == nil {
		t.Error("verifyProcOwnedByEuid(1) returned nil; expected mismatch error for root-owned PID 1")
	}
}

// TestVerifyProcOwnedByEuid_NonLinux is a placeholder: on non-Linux platforms
// the helper is a no-op and must not return an error for any PID.
func TestVerifyProcOwnedByEuid_NonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux-specific path covered by other tests")
	}
	if err := verifyProcOwnedByEuid(os.Getpid()); err != nil {
		t.Errorf("verifyProcOwnedByEuid on non-linux should be no-op, got %v", err)
	}
}

// Auto-takeover kills the terminal CLI only when the adopted Claude transcript
// can resume on claude: an IM agent pinned to another backend, or a deployment
// without claude, would trade the user's live session for an unrelated fresh one.
func TestTryAutoTakeover_KillsOnlyForAClaudeResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("discovery is POSIX-only")
	}
	cases := []struct {
		name, backend, agentBackend string
		wantKill                    bool
	}{
		{name: "claude resume", backend: "claude", wantKill: true},
		{name: "agent pinned to kiro", backend: "claude", agentBackend: "kiro"},
		{name: "no claude backend", backend: "kiro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Skipf("cannot start child: %v", err)
			}
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })

			claudeDir, ws := t.TempDir(), t.TempDir()
			live, _ := json.Marshal(map[string]any{
				"pid": cmd.Process.Pid, "sessionId": "0b8f3c2e-5d7a-4e1b-9c6f-2a4d8e1f3b5c",
				"cwd": ws, "startedAt": time.Now().UnixMilli(), "entrypoint": "cli",
			})
			if err := os.MkdirAll(filepath.Join(claudeDir, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(claudeDir, "sessions", "1.json"), live, 0o600); err != nil {
				t.Fatal(err)
			}
			w := cli.NewWrapperLazy("/nonexistent/"+tc.backend, &cli.ClaudeProtocol{}, tc.backend)
			router := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1})
			t.Cleanup(router.Shutdown)
			s := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: tc.backend})
			s.claudeDir = claudeDir

			opts := session.AgentOpts{Workspace: ws, Backend: tc.agentBackend}
			if s.tryAutoTakeover(context.Background(), "test:direct:u1:general", "test:direct:u1:general", opts) {
				t.Fatal("the takeover reported success with a CLI that cannot spawn")
			}
			select {
			case <-exited:
				if !tc.wantKill {
					t.Error("the terminal CLI was killed for a takeover that cannot resume it")
				}
			case <-time.After(200 * time.Millisecond):
				if tc.wantKill {
					t.Error("the terminal CLI survived a takeover that resumes it on claude")
				}
			}
		})
	}
}
