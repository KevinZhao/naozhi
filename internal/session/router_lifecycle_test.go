package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
// the session table. No guard channel may be left dangling either: the gate
// ends a guard the caller installed, which "ResetAndRecreate stopped during
// close" pins for ResetAndRecreate and TestTakeover_EndsTheLeaseOnFailure
// ("router stopped meanwhile") for a TakeoverLease.
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
		_, err := reserveAndTakeover(context.Background(), r, "feishu:p2p:u2", "sess-abc", "", AgentOpts{})
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

	// The router stops while ResetAndRecreate has the lock released to close
	// the old process, after it installed its own in-flight guard.
	t.Run("ResetAndRecreate stopped during close", func(t *testing.T) {
		t.Parallel()
		const key = "feishu:p2p:u4"
		r := newStoppedGateRouter()
		injectSession(r, key, newHookCloseProc(func() { r.stopped.Store(true) }))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := r.ResetAndRecreate(ctx, key, AgentOpts{})
		if !errors.Is(err, ErrRouterStopped) {
			t.Fatalf("ResetAndRecreate err = %v, want ErrRouterStopped", err)
		}
		if _, held := spawnInFlight(r, key); held {
			t.Errorf("%s still has the reset's in-flight marker", key)
		}
		assertNoLeak(t, r)
		// A dangling marker would park this call until ctx expires.
		start := time.Now()
		if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); !errors.Is(err, ErrRouterStopped) {
			t.Errorf("follow-up GetOrCreate err = %v after %v, want ErrRouterStopped", err, time.Since(start))
		}
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

// TestGetOrCreate_YieldedSpawnIsNotResumeLost: a resume whose transcript is
// gone but whose spawn yields to a live session installed meanwhile returns
// that session, which lost nothing, so the status is not SessionResumeLost.
func TestGetOrCreate_YieldedSpawnIsNotResumeLost(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:alice:general"
	r := newResumeGuardRouter(t)
	g := newGatedSpawn()
	r.spawn.hook = g.hook
	dead := injectSession(r, key, newDeadProc())
	dead.setWorkspace("/home/u/proj")
	dead.setSessionID("sess-1")

	type result struct {
		s   *ManagedSession
		st  SessionStatus
		err error
	}
	out := make(chan result, 1)
	go func() {
		s, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
		out <- result{s, st, err}
	}()
	waitEntered(t, g)
	winner := injectSession(r, key, newIdleProc())
	close(g.release)

	var got result
	select {
	case got = <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("GetOrCreate did not return")
	}
	if got.err != nil || got.s != winner {
		t.Fatalf("GetOrCreate = %p, %v; want the session installed meanwhile", got.s, got.err)
	}
	if got.st == SessionResumeLost {
		t.Error("status = SessionResumeLost for a session this spawn did not install")
	}
}
