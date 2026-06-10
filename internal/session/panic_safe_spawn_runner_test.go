package session

// Pins the Runner-seam migration of panicSafeSpawn (agentcore-cloud-sandbox
// RFC §4.2): the helper now takes cli.Runner instead of *cli.Wrapper. Two
// contracts matter at this seam:
//
//  1. nil runner → descriptive error, no panic. (*cli.Wrapper)(nil).Runner()
//     returns nil, so any future caller that drops the upstream nil-wrapper
//     guard still fails soft here instead of nil-dereferencing.
//  2. A typed-nil Runner inside a non-nil interface is NOT the nil branch —
//     it reaches Spawn and the panic there is absorbed by panicSafeSpawnFn's
//     recover (RES1), surfacing as a regular error.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func TestPanicSafeSpawn_NilRunnerFailsSoft(t *testing.T) {
	t.Parallel()

	proc, err := panicSafeSpawn(context.Background(), nil, cli.SpawnOptions{}, "k1", "claude")
	if proc != nil {
		t.Errorf("proc = %v, want nil", proc)
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil for nil runner")
	}
	if !strings.Contains(err.Error(), "no runner") {
		t.Errorf("err = %q, want substring %q", err.Error(), "no runner")
	}
	// Classification contract: a nil runner means "no spawnable backend" —
	// the same condition the upstream nil-wrapper guard reports — so it must
	// wrap ErrNoCLIWrapper for usermsg/classify to map it to the same
	// user-facing error code instead of a generic fallback.
	if !errors.Is(err, ErrNoCLIWrapper) {
		t.Errorf("errors.Is(err, ErrNoCLIWrapper) = false, want true — nil runner must classify like nil wrapper")
	}
}

func TestPanicSafeSpawn_NilWrapperRunnerFailsSoft(t *testing.T) {
	t.Parallel()

	// The exact expression spawnSession would evaluate if its nil-wrapper
	// guard were ever removed: (*cli.Wrapper)(nil).Runner() is an untyped
	// nil interface, so the helper's nil branch must catch it.
	var w *cli.Wrapper
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped panicSafeSpawn for nil wrapper runner: %v", r)
		}
	}()
	_, err := panicSafeSpawn(context.Background(), w.Runner(), cli.SpawnOptions{}, "k1", "claude")
	if err == nil {
		t.Fatal("err = nil, want non-nil for nil-wrapper runner")
	}
}
