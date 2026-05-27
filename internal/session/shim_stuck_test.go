package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestErrShimStuck_WrapWalksChain pins the load-bearing contract: callers
// like the cron freshContextPreflightP0 path can errors.Is(err, ErrShimStuck)
// even when the spawn error is wrapped underneath. The doubly-wrapped form
// (fmt.Errorf("%w: %w", ErrShimStuck, spawnErr)) used inside GetOrCreate /
// ResetAndRecreate must surface ErrShimStuck through the standard chain
// walker — without this the cron classifier falls back to the generic
// session_error class and the operator sees "执行跳过，请稍后重试。"
// instead of an actionable diagnosis. (#1324)
func TestErrShimStuck_WrapWalksChain(t *testing.T) {
	t.Parallel()
	spawnErr := errors.New("refusing to clobber existing socket /run/user/1000/abc")
	wrapped := fmt.Errorf("%w: %w", ErrShimStuck, spawnErr)
	if !errors.Is(wrapped, ErrShimStuck) {
		t.Error("errors.Is(wrapped, ErrShimStuck) = false; the chain walk failed")
	}
	if !errors.Is(wrapped, spawnErr) {
		t.Error("errors.Is(wrapped, spawnErr) = false; underlying spawn error was lost")
	}
}

// TestErrShimStuck_DistinctFromGenericSpawn confirms the sentinel is not
// satisfied by an unrelated spawn error — guarding against an over-broad
// classifier that would trigger ErrShimStuck branding for every spawn
// failure (e.g. shim binary missing, OOM, panic recovery).
func TestErrShimStuck_DistinctFromGenericSpawn(t *testing.T) {
	t.Parallel()
	other := errors.New("shim binary missing on PATH")
	if errors.Is(other, ErrShimStuck) {
		t.Error("a plain spawn error must NOT match ErrShimStuck")
	}
	wrapped := fmt.Errorf("session foo: %w", other)
	if errors.Is(wrapped, ErrShimStuck) {
		t.Error("non-stuck wrapped spawn error must NOT match ErrShimStuck")
	}
}

// TestRouter_ShimStuckFlagConsumedByGetOrCreate exercises the per-key flag
// lifecycle: setting shimStuckOnReset[key] before GetOrCreate causes the
// returned spawn error to wrap ErrShimStuck, AND the flag is cleared so a
// follow-up GetOrCreate gets a clean (non-ErrShimStuck) error if it fails
// for a different reason. Without the consume step, every retry would be
// branded ErrShimStuck even after the shim freed up.
func TestRouter_ShimStuckFlagConsumedByGetOrCreate(t *testing.T) {
	t.Parallel()
	// Build a minimal Router with no wrapper — spawnSession will fail
	// quickly. We don't need a real CLI; we just need the error path to
	// run with the stuck flag set.
	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		spawningKeys:       make(map[string]chan struct{}),
		shimStuckOnReset:   make(map[string]bool),
		workspaceOverrides: make(map[string]string),
		backendOverrides:   make(map[string]string),
		knownIDs:           make(map[string]bool),
		sessionIDToKey:     make(map[string]string),
	}
	const key = "stuck:key:test"
	r.shimStuckOnReset[key] = true

	_, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err == nil {
		t.Fatal("expected GetOrCreate error (no wrapper wired)")
	}
	if !errors.Is(err, ErrShimStuck) {
		t.Errorf("first GetOrCreate err did not wrap ErrShimStuck: %v", err)
	}
	if _, still := r.shimStuckOnReset[key]; still {
		t.Error("shimStuckOnReset[key] still set after GetOrCreate; flag must be consumed")
	}

	// Second call without the flag: error must NOT wrap ErrShimStuck.
	_, _, err2 := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err2 == nil {
		t.Fatal("expected second GetOrCreate error (still no wrapper)")
	}
	if errors.Is(err2, ErrShimStuck) {
		t.Errorf("second GetOrCreate must NOT wrap ErrShimStuck (flag was consumed): %v", err2)
	}
}

// TestRouter_ShimStuckFlagPerKey isolates per-key state — flagging key A
// must not affect classifier behaviour for key B. Without this isolation
// a single stuck shim would brand every cron job's next run with the
// stuck class, hiding actual unrelated session_error failures.
func TestRouter_ShimStuckFlagPerKey(t *testing.T) {
	t.Parallel()
	r := &Router{
		sessions:           make(map[string]*ManagedSession),
		spawningKeys:       make(map[string]chan struct{}),
		shimStuckOnReset:   make(map[string]bool),
		workspaceOverrides: make(map[string]string),
		backendOverrides:   make(map[string]string),
		knownIDs:           make(map[string]bool),
		sessionIDToKey:     make(map[string]string),
	}
	const stuckKey = "key:A"
	const cleanKey = "key:B"
	r.shimStuckOnReset[stuckKey] = true

	_, _, errClean := r.GetOrCreate(context.Background(), cleanKey, AgentOpts{})
	if errClean == nil {
		t.Fatal("expected GetOrCreate(cleanKey) error")
	}
	if errors.Is(errClean, ErrShimStuck) {
		t.Errorf("clean key got ErrShimStuck wrap: %v", errClean)
	}
	if !r.shimStuckOnReset[stuckKey] {
		t.Error("stuckKey flag must remain after GetOrCreate(cleanKey)")
	}
}
