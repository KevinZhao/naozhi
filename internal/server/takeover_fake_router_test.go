package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

var errFakeRefused = errors.New("fake router refuses the takeover")

// fakeServerRouter is a serverRouter that records the calls tryAutoTakeover
// makes and refuses every ReserveTakeover, so no test here reaches the kill.
type fakeServerRouter struct {
	backends    *session.BackendRegistry
	existing    bool   // SessionFor reports a managed session
	workspace   string // Workspace's answer
	excludePIDs map[int]bool
	calls       []string
	reserved    session.AgentOpts
}

func (f *fakeServerRouter) Backends() *session.BackendRegistry { return f.backends }

func (f *fakeServerRouter) SessionFor(key string) *session.ManagedSession {
	f.calls = append(f.calls, "SessionFor")
	if f.existing {
		return &session.ManagedSession{}
	}
	return nil
}

func (f *fakeServerRouter) Workspace(string) string {
	f.calls = append(f.calls, "Workspace")
	return f.workspace
}

func (f *fakeServerRouter) ManagedExcludeSets() (map[int]bool, map[string]bool, map[string]bool) {
	f.calls = append(f.calls, "ManagedExcludeSets")
	return f.excludePIDs, nil, nil
}

func (f *fakeServerRouter) ReserveTakeover(_ string, opts session.AgentOpts) (*session.TakeoverLease, error) {
	f.calls = append(f.calls, "ReserveTakeover")
	f.reserved = opts
	return nil, errFakeRefused
}

func (f *fakeServerRouter) Takeover(context.Context, *session.TakeoverLease, string, string) (*session.ManagedSession, error) {
	f.calls = append(f.calls, "Takeover")
	return nil, errFakeRefused
}

func (f *fakeServerRouter) Remove(string) bool { return false }
func (f *fakeServerRouter) BumpVersion()       {}

// tryAutoTakeover depends on the router only through serverRouter, so each of
// its gates can be driven by a fake: no claude dir, a key that already has a
// session, no workspace, an excluded (managed) PID, and a candidate that
// reaches the reservation with its CWD as the workspace.
func TestTryAutoTakeover_FakeRouterGates(t *testing.T) {
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
	pid := cmd.Process.Pid

	claudeDir, ws := t.TempDir(), t.TempDir()
	live, _ := json.Marshal(map[string]any{
		"pid": pid, "sessionId": "0b8f3c2e-5d7a-4e1b-9c6f-2a4d8e1f3b5c",
		"cwd": ws, "startedAt": time.Now().UnixMilli(), "entrypoint": "cli",
	})
	if err := os.MkdirAll(filepath.Join(claudeDir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "sessions", "1.json"), live, 0o600); err != nil {
		t.Fatal(err)
	}
	// The registry is a session value type; a router only mints it here.
	claude := session.NewRouter(session.RouterConfig{
		Wrapper: cli.NewWrapperLazy("/nonexistent/claude", &cli.ClaudeProtocol{}, "claude"), MaxProcs: 1,
	})
	t.Cleanup(claude.Shutdown)

	const key = "test:direct:u1:general"
	cases := []struct {
		name      string
		claudeDir string
		router    fakeServerRouter
		opts      session.AgentOpts
		wantCalls string
	}{
		{name: "no claude dir", router: fakeServerRouter{}, opts: session.AgentOpts{Workspace: ws}},
		{name: "session exists", claudeDir: claudeDir, router: fakeServerRouter{existing: true},
			opts: session.AgentOpts{Workspace: ws}, wantCalls: "SessionFor"},
		{name: "no workspace", claudeDir: claudeDir, router: fakeServerRouter{},
			wantCalls: "SessionFor,Workspace"},
		{name: "managed pid excluded", claudeDir: claudeDir, router: fakeServerRouter{excludePIDs: map[int]bool{pid: true}},
			opts: session.AgentOpts{Workspace: ws}, wantCalls: "SessionFor,ManagedExcludeSets"},
		{name: "chat workspace reaches the reservation", claudeDir: claudeDir, router: fakeServerRouter{workspace: ws},
			wantCalls: "SessionFor,Workspace,ManagedExcludeSets,ReserveTakeover"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.router
			fake.backends = claude.Backends()
			s := &Server{router: &fake, claudeDir: tc.claudeDir}
			if s.tryAutoTakeover(context.Background(), key, key, tc.opts) {
				t.Fatal("takeover reported success against a router that refuses it")
			}
			if got := strings.Join(fake.calls, ","); got != tc.wantCalls {
				t.Errorf("router calls = %q, want %q", got, tc.wantCalls)
			}
			if strings.HasSuffix(tc.wantCalls, "ReserveTakeover") && fake.reserved.Workspace != ws {
				t.Errorf("reserved with workspace %q, want the candidate's CWD %q", fake.reserved.Workspace, ws)
			}
		})
	}
	select {
	case <-exited:
		t.Fatal("the terminal CLI was killed although every takeover was refused")
	default:
	}
}
