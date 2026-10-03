package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
)

// newStoppedGateRouter builds a fully map-initialized Router (via NewRouter, so
// spawningKeys / sessions are non-nil; wsStore is zero-value usable) wired to a
// non-existent CLI binary. The stopped gate in the spawn returns before any
// real spawn, so the bogus binary is never executed.
func newStoppedGateRouter() *Router {
	return NewRouter(RouterConfig{
		MaxProcs: 3,
		Wrapper:  cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
	})
}

// TestSpawnSession_RejectedAfterStopped pins the #1822 (Option B) stopped gate:
// once r.stopped is set (which Router.shutdown does under the table lock before snapshotting
// sessions), every reverse-RPC spawn path that funnels into the spawn —
// GetOrCreate (send), Takeover (takeover), ResetAndRecreate (restart_planner) —
// must refuse with ErrRouterStopped and must NOT install a fresh session into
// the session table (the leak the issue is about). The gate sits before the spawningKeys
// lazy-init/defer, so no guard channel may be left dangling either.
func TestSpawnSession_RejectedAfterStopped(t *testing.T) {
	t.Parallel()

	assertNoLeak := func(t *testing.T, r *Router) {
		t.Helper()
		if lenT(r) != 0 {
			t.Errorf("the session table grew to %d after a rejected spawn; gate must run before any map mutation", lenT(r))
		}
		if stateOf(r).spawns.SpawningCount() != 0 {
			t.Errorf("stateOf(r).spawns.SpawningCount() = %d; gate must sit before spawningKeys lazy-init so no guard channel is left dangling", stateOf(r).spawns.SpawningCount())
		}
	}

	t.Run("GetOrCreate", func(t *testing.T) {
		t.Parallel()
		r := newStoppedGateRouter()
		r.stopped.Store(true)
		_, _, err := r.GetOrCreate(context.Background(), "feishu:p2p:u1", AgentOpts{})
		if !errors.Is(err, ErrRouterStopped) {
			t.Fatalf("GetOrCreate err = %v, want ErrRouterStopped", err)
		}
		assertNoLeak(t, r)
	})

	t.Run("Takeover", func(t *testing.T) {
		t.Parallel()
		r := newStoppedGateRouter()
		r.stopped.Store(true)
		_, err := r.Takeover(context.Background(), "feishu:p2p:u2", "sess-abc", "", AgentOpts{})
		if !errors.Is(err, ErrRouterStopped) {
			t.Fatalf("Takeover err = %v, want ErrRouterStopped", err)
		}
		assertNoLeak(t, r)
	})

	t.Run("ResetAndRecreate", func(t *testing.T) {
		t.Parallel()
		r := newStoppedGateRouter()
		r.stopped.Store(true)
		_, err := r.ResetAndRecreate(context.Background(), "feishu:p2p:u3", AgentOpts{})
		if !errors.Is(err, ErrRouterStopped) {
			t.Fatalf("ResetAndRecreate err = %v, want ErrRouterStopped", err)
		}
		assertNoLeak(t, r)
	})
}

// newResumeGuardRouter is a router whose resume guard probes an empty scratch
// claude dir.
func newResumeGuardRouter(t *testing.T) *Router {
	t.Helper()
	r := NewRouter(RouterConfig{
		MaxProcs:  3,
		Wrapper:   cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
		ClaudeDir: t.TempDir(),
	})
	t.Cleanup(r.Shutdown)
	return r
}

// TestGetOrCreate_ReportsADroppedResumeTarget: a dead session with an ID
// whose transcript is gone respawns fresh and reports SessionResumeLost; with
// the transcript on disk, or with no ID to resume, it stays SessionResumed.
func TestGetOrCreate_ReportsADroppedResumeTarget(t *testing.T) {
	t.Parallel()
	const key, ws, sid = "feishu:direct:alice:general", "/home/u/proj", "sess-1"
	cases := []struct {
		name       string
		sessionID  string
		transcript bool
		want       SessionStatus
		wantResume string
	}{
		{"transcript present", sid, true, SessionResumed, sid},
		{"transcript missing", sid, false, SessionResumeLost, ""},
		{"no session id", "", false, SessionResumed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newResumeGuardRouter(t)
			if tc.transcript {
				jsonl := claudefs.SessionJSONL(r.hist.claudeDir, ws, sid)
				if err := os.MkdirAll(filepath.Dir(jsonl), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(jsonl, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var resumeID string
			r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
				resumeID = opts.ResumeID
				return newIdleProc(), nil
			}
			dead := injectSession(r, key, newDeadProc())
			dead.setWorkspace(ws)
			dead.setSessionID(tc.sessionID)

			_, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if st != tc.want || resumeID != tc.wantResume {
				t.Errorf("status = %d, spawn resume = %q; want %d, %q", st, resumeID, tc.want, tc.wantResume)
			}
		})
	}
	t.Run("fresh key", func(t *testing.T) {
		t.Parallel()
		r := newResumeGuardRouter(t)
		r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil }
		if _, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{}); err != nil || st != SessionNew {
			t.Errorf("status = %d, err = %v; want SessionNew", st, err)
		}
	})
}
