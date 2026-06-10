package session

// Pins the classification contract of the UPSTREAM nil-wrapper guard in
// spawnSession (router_lifecycle.go). Review of the Runner-seam PR found an
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
)

func TestSpawnSession_NoWrapperWrapsErrNoCLIWrapper(t *testing.T) {
	t.Parallel()

	// A router with zero wrappers: wrapperFor resolves nil for any backend,
	// so GetOrCreate → spawnSession reaches the nil-wrapper guard.
	r := NewRouter(RouterConfig{})

	_, _, err := r.GetOrCreate(context.Background(), "feishu:p2p:classify-pin", AgentOpts{})
	if err == nil {
		t.Fatal("GetOrCreate with no wrappers: err = nil, want non-nil")
	}
	if !errors.Is(err, ErrNoCLIWrapper) {
		t.Errorf("errors.Is(err, ErrNoCLIWrapper) = false, want true — upstream guard must classify like the nil-runner guard (err=%q)", err)
	}
}
