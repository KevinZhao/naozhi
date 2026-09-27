package session

import (
	"context"
	"errors"
	"testing"

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
