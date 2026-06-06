package session

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// newStoppedGateRouter builds a fully map-initialized Router (via NewRouter, so
// wsStore.overrides / spawningKeys / sessions are all non-nil) wired to a
// non-existent CLI binary. The stopped gate in spawnSession returns before any
// real spawn, so the bogus binary is never executed.
func newStoppedGateRouter() *Router {
	return NewRouter(RouterConfig{
		MaxProcs: 3,
		Wrapper:  cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
	})
}

// TestSpawnSession_RejectedAfterStopped pins the #1822 (Option B) stopped gate:
// once r.stopped is set (which Router.shutdown does under r.mu before snapshotting
// sessions), every reverse-RPC spawn path that funnels into spawnSession —
// GetOrCreate (send), Takeover (takeover), ResetAndRecreate (restart_planner) —
// must refuse with ErrRouterStopped and must NOT install a fresh session into
// r.sessions (the leak the issue is about). The gate sits before the spawningKeys
// lazy-init/defer, so no guard channel may be left dangling either.
func TestSpawnSession_RejectedAfterStopped(t *testing.T) {
	t.Parallel()

	assertNoLeak := func(t *testing.T, r *Router) {
		t.Helper()
		if len(r.sessions) != 0 {
			t.Errorf("r.sessions grew to %d after a rejected spawn; gate must run before any map mutation", len(r.sessions))
		}
		if len(r.spawningKeys) != 0 {
			t.Errorf("r.spawningKeys = %d; gate must sit before spawningKeys lazy-init so no guard channel is left dangling", len(r.spawningKeys))
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
