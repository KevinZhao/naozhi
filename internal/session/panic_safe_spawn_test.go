package session

// RES1 regression tests. A wrapper.Spawn panic (shim exec crash, broken
// protocol Init, etc.) used to leave r.pendingSpawns incremented forever
// because the `r.pendingSpawns--` line after wrapper.Spawn never ran.
// Every subsequent GetOrCreate then hit the `activeCount+pendingSpawns
// >= maxProcs` gate and returned ErrMaxProcs until the process restarted.
//
// panicSafeSpawnFn wraps the spawn call in a deferred recover that
// converts panic → error. These tests inject a panicking spawnerFunc
// (the real *cli.Wrapper has no panic-injection seam) and confirm that
// the helper:
//   1. Returns a non-nil error when the spawn panics.
//   2. Does not re-panic past its own frame.
//   3. Leaves the normal (non-panic) path unchanged.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestPanicSafeSpawnFn_RecoversPanic(t *testing.T) {
	t.Parallel()

	spawn := func(context.Context, cli.SpawnOptions) (*cli.Process, error) {
		panic("synthetic spawn panic")
	}

	// Must not re-panic past the helper — if it does, this defer never fires.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped panicSafeSpawnFn: %v", r)
		}
	}()

	proc, err := panicSafeSpawnFn(context.Background(), spawn, cli.SpawnOptions{}, "k1", "claude")
	if proc != nil {
		t.Errorf("proc = %v, want nil on panic", proc)
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil on panic")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("err = %q, want substring %q", err.Error(), "panic")
	}
}

func TestPanicSafeSpawnFn_PassesThroughHappyPath(t *testing.T) {
	t.Parallel()

	// A successful spawn returns (nil, nil) in this stub — we only care
	// that the wrapper does not add spurious errors to the happy path.
	spawn := func(context.Context, cli.SpawnOptions) (*cli.Process, error) {
		return nil, nil
	}
	proc, err := panicSafeSpawnFn(context.Background(), spawn, cli.SpawnOptions{}, "k1", "claude")
	if proc != nil {
		t.Errorf("proc = %v, want nil (stub returns nil)", proc)
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestPanicSafeSpawnFn_PassesThroughRegularError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("upstream shim exec failed")
	spawn := func(context.Context, cli.SpawnOptions) (*cli.Process, error) {
		return nil, sentinel
	}
	_, err := panicSafeSpawnFn(context.Background(), spawn, cli.SpawnOptions{}, "k1", "claude")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is sentinel (regular error must not be wrapped)", err)
	}
}

// TestPanicSafeSpawnFn_NilSpawnPanicIsRecovered pins behaviour for the
// "nil wrapper was never meant to reach this path" guard: if someone
// accidentally calls the helper with a nil spawn, the panic must be
// captured rather than crashing the process.
func TestPanicSafeSpawnFn_NilSpawnPanicIsRecovered(t *testing.T) {
	t.Parallel()

	// Pass nil where a spawnerFunc is expected: calling it panics, and
	// panicSafeSpawnFn's recover must absorb that.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped panicSafeSpawnFn: %v", r)
		}
	}()
	_, err := panicSafeSpawnFn(context.Background(), nil, cli.SpawnOptions{}, "k1", "claude")
	if err == nil {
		t.Fatal("err = nil, want non-nil (nil-spawn panic)")
	}
}
