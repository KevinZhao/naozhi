package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
		sessions:         make(map[string]*ManagedSession),
		spawningKeys:     make(map[string]chan struct{}),
		shimStuckOnReset: make(map[string]bool),
		kid:              knownIDsStore{ids: make(map[string]bool)},
		sessionIDToKey:   make(map[string]string),
	}
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.backendOverrides = make(map[string]string)
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
		sessions:         make(map[string]*ManagedSession),
		spawningKeys:     make(map[string]chan struct{}),
		shimStuckOnReset: make(map[string]bool),
		kid:              knownIDsStore{ids: make(map[string]bool)},
		sessionIDToKey:   make(map[string]string),
	}
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.backendOverrides = make(map[string]string)
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

// TestRouter_ShimStuckFlagClearedOnTerminalRemoval verifies R090031-CR-5:
// unregisterSessionLocked with keepBackendOverride=false (terminal removal path)
// must delete shimStuckOnReset[key] so permanently-deleted sessions do not
// accumulate stale map entries for the lifetime of the process.
func TestRouter_ShimStuckFlagClearedOnTerminalRemoval(t *testing.T) {
	t.Parallel()
	const key = "dead:session:key"
	r := &Router{
		sessions:         make(map[string]*ManagedSession),
		spawningKeys:     make(map[string]chan struct{}),
		shimStuckOnReset: make(map[string]bool),
		kid:              knownIDsStore{ids: make(map[string]bool)},
		sessionIDToKey:   make(map[string]string),
	}
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.backendOverrides = make(map[string]string)
	s := &ManagedSession{key: key}
	r.sessions[key] = s
	r.shimStuckOnReset[key] = true

	r.mu.Lock()
	r.unregisterSessionLocked(key, s, false)
	r.mu.Unlock()

	if _, found := r.shimStuckOnReset[key]; found {
		t.Error("shimStuckOnReset[key] must be deleted on terminal removal (keepBackendOverride=false)")
	}
}

// TestWarnShimStuckReuse_EmitsOnStuck pins the #1702 fix: when the success
// path of ResetAndRecreate consumes a set stuck flag (spawnSession reused an
// alive session via the TOCTOU guard and returned err==nil), the stuck
// diagnostic must NOT be silently dropped — it must surface as a Warn so
// operators still learn the shim socket was bound after the gone-wait.
func TestWarnShimStuckReuse_EmitsOnStuck(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	const key = "feishu:direct:stuck-reuse:general"
	warnShimStuckReuse(true, key)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a WARN record on the stuck-reuse path, got: %q", out)
	}
	if !strings.Contains(out, key) {
		t.Errorf("warn must include the session key for operator triage, got: %q", out)
	}
}

// TestWarnShimStuckReuse_SilentWhenNotStuck confirms the normal success path
// (flag not set) emits nothing — we must not spam a warn on every reset.
func TestWarnShimStuckReuse_SilentWhenNotStuck(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	warnShimStuckReuse(false, "feishu:direct:clean:general")

	if out := buf.String(); out != "" {
		t.Errorf("non-stuck success path must not emit any log, got: %q", out)
	}
}

// TestRouter_ShimStuckFlagPreservedOnKeepOverride verifies that
// unregisterSessionLocked with keepBackendOverride=true (ResetAndRecreate /
// Takeover path) does NOT delete shimStuckOnReset[key] — the key is being
// recycled so the stuck flag must survive to be consumed by the next spawn.
func TestRouter_ShimStuckFlagPreservedOnKeepOverride(t *testing.T) {
	t.Parallel()
	const key = "recycled:session:key"
	r := &Router{
		sessions:         make(map[string]*ManagedSession),
		spawningKeys:     make(map[string]chan struct{}),
		shimStuckOnReset: make(map[string]bool),
		kid:              knownIDsStore{ids: make(map[string]bool)},
		sessionIDToKey:   make(map[string]string),
	}
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.backendOverrides = make(map[string]string)
	s := &ManagedSession{key: key}
	r.sessions[key] = s
	r.shimStuckOnReset[key] = true

	r.mu.Lock()
	r.unregisterSessionLocked(key, s, true)
	r.mu.Unlock()

	if !r.shimStuckOnReset[key] {
		t.Error("shimStuckOnReset[key] must survive unregisterSessionLocked with keepBackendOverride=true")
	}
}
