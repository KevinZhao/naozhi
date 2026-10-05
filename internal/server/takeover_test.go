package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
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
		{name: "chat pinned to claude", backend: "claude", agentBackend: "claude", wantKill: true},
		{name: "chat pinned to kiro", backend: "claude", agentBackend: "kiro"},
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

// A takeover the router would refuse leaves the terminal CLI running (#3395):
// the precheck runs before SIGTERM, so the child dies only by the test's own
// SIGKILL.
func TestTryAutoTakeover_RefusedTakeoverLeavesCLIAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("discovery is POSIX-only")
	}
	cases := []struct {
		name  string
		model string
		setup func(r *session.Router)
	}{
		{name: "router stopped", setup: func(r *session.Router) { r.Shutdown() }},
		{name: "invalid model", model: "--bad", setup: func(*session.Router) {}},
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
			w := cli.NewWrapperLazy("/nonexistent/claude", &cli.ClaudeProtocol{}, "claude")
			router := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1})
			t.Cleanup(router.Shutdown)
			s := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
			s.claudeDir = claudeDir
			tc.setup(router)

			opts := session.AgentOpts{Workspace: ws, Model: tc.model}
			if s.tryAutoTakeover(context.Background(), "test:direct:u1:general", "test:direct:u1:general", opts) {
				t.Fatal("a refused takeover reported success")
			}
			select {
			case <-exited:
				t.Fatal("the terminal CLI was killed for a takeover the router refuses")
			case <-time.After(300 * time.Millisecond):
			}
			_ = cmd.Process.Kill()
			<-exited
			if st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || st.Signal() != syscall.SIGKILL {
				t.Errorf("child ended with %v, want the test's SIGKILL", cmd.ProcessState)
			}
		})
	}
}

// TestDashboardTakeover_InvalidAgentModelLeavesCLIAlive pins #3395 for the
// dashboard: the precheck sees the general agent's model that Takeover will
// be given, so a model the router rejects is refused before the SIGTERM.
func TestDashboardTakeover_InvalidAgentModelLeavesCLIAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("discovery is POSIX-only")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	start, err := discovery.ProcStartTime(cmd.Process.Pid)
	if err != nil {
		t.Skipf("cannot read child start time: %v", err)
	}

	const sid = "0b8f3c2e-5d7a-4e1b-9c6f-2a4d8e1f3b5c"
	router := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(router.Shutdown)
	s := NewWithOptions(ServerOptions{
		Addr: ":0", Router: router, Backend: "claude",
		Agents: map[string]session.AgentOpts{"general": {Model: "--bad"}},
	})
	s.discoveryH.SetClaudeDirForTest(t.TempDir())
	s.discoveryCache.sessions = []discovery.DiscoveredSession{{PID: cmd.Process.Pid, SessionID: sid}}

	body := fmt.Sprintf(`{"pid":%d,"session_id":%q,"cwd":%q,"proc_start_time":%d}`, cmd.Process.Pid, sid, t.TempDir(), start)
	w := httptest.NewRecorder()
	s.discoveryH.HandleTakeover(w, httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", strings.NewReader(body)))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d (%q), want 503 from the precheck", w.Code, strings.TrimSpace(w.Body.String()))
	}
	select {
	case <-exited:
		t.Fatal("the external CLI was killed for a takeover the router refuses")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestTryAutoTakeover_HoldsTheKeyWhileTheCLIExits pins #3417 for IM: the key
// is reserved before the SIGTERM, so while the terminal CLI exits another
// takeover of it is refused, and the key is free again once the takeover ends.
func TestTryAutoTakeover_HoldsTheKeyWhileTheCLIExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("discovery is POSIX-only")
	}
	const key = "test:direct:u1:general"
	termed := filepath.Join(t.TempDir(), "termed")
	// Survives SIGTERM, recording it, so the exit wait lasts until ctx ends.
	cmd := exec.Command("sh", "-c", `trap 'echo > "$0"' TERM; while :; do sleep 0.05; done`, termed)
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
	w := cli.NewWrapperLazy("/nonexistent/claude", &cli.ClaudeProtocol{}, "claude")
	router := session.NewRouter(session.RouterConfig{Wrapper: w, MaxProcs: 1})
	t.Cleanup(router.Shutdown)
	s := NewWithOptions(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	s.claudeDir = claudeDir

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- s.tryAutoTakeover(ctx, key, key, session.AgentOpts{Workspace: ws}) }()
	testhelper.Eventually(t, func() bool {
		_, err := os.Stat(termed)
		return err == nil
	}, 5*time.Second, "the terminal CLI never got its SIGTERM")
	if _, err := router.ReserveTakeover(key, session.AgentOpts{}); !errors.Is(err, session.ErrSpawnInFlight) {
		t.Errorf("ReserveTakeover during the exit wait = %v, want ErrSpawnInFlight", err)
	}
	cancel()
	if <-done {
		t.Fatal("the takeover reported success with a CLI that cannot spawn")
	}
	lease, err := router.ReserveTakeover(key, session.AgentOpts{})
	if err != nil {
		t.Fatalf("ReserveTakeover after the takeover ended: %v", err)
	}
	lease.Release()
}
