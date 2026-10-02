package session

// Pins the classification contract of the UPSTREAM nil-wrapper guard in
// the spawn (router_lifecycle.go). Review of the Runner-seam PR found an
// asymmetry: the new nil-runner guard in panicSafeSpawn has a test asserting
// it wraps ErrNoCLIWrapper (panic_safe_spawn_runner_test.go), but the
// pre-existing guard it mirrors had none — a silent edit there (wrapping a
// different sentinel, or dropping the %w) would divert usermsg/classify to a
// generic error code without failing any test. The two guards must stay
// classification-equivalent: both mean "no spawnable backend".

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestSpawnSession_NoWrapperWrapsErrNoCLIWrapper(t *testing.T) {
	t.Parallel()

	// A router with zero wrappers: wrapperFor resolves nil for any backend,
	// so GetOrCreate → the spawn reaches the nil-wrapper guard.
	r := NewRouter(RouterConfig{})

	_, _, err := r.GetOrCreate(context.Background(), "feishu:p2p:classify-pin", AgentOpts{})
	if err == nil {
		t.Fatal("GetOrCreate with no wrappers: err = nil, want non-nil")
	}
	if !errors.Is(err, ErrNoCLIWrapper) {
		t.Errorf("errors.Is(err, ErrNoCLIWrapper) = false, want true — upstream guard must classify like the nil-runner guard (err=%q)", err)
	}
}

// TestSpawnSession_NoWrapperRefusesEvenWithAHook: a spawn hook does not
// stand in for the wrapper. installFreshSession reads the wrapper's CLI name
// and version, so a guard that let a hook through without one would trade
// this classified error for a nil dereference.
func TestSpawnSession_NoWrapperRefusesEvenWithAHook(t *testing.T) {
	t.Parallel()

	r := NewRouter(RouterConfig{})
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil }

	_, _, err := r.GetOrCreate(context.Background(), "feishu:p2p:hook-no-wrapper", AgentOpts{})
	if !errors.Is(err, ErrNoCLIWrapper) {
		t.Errorf("GetOrCreate with a hook and no wrapper: err = %v, want ErrNoCLIWrapper", err)
	}
}
