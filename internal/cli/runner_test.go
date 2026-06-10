package cli

// Runner seam tests (agentcore-cloud-sandbox RFC §4.2). The localRunner is a
// pure extraction of (*Wrapper).Spawn behind the placement interface — these
// tests pin (1) the nil-receiver contract that session.spawnSession's
// nil-wrapper guard relies on, (2) the placement constant, and (3) that
// Spawn is pure delegation: identical error text to calling wrapper.Spawn
// directly, so the extraction is provably behaviour-preserving on the
// no-shim-manager path (the only Spawn path exercisable without a live shim).

import (
	"context"
	"strings"
	"testing"
)

func TestWrapperRunner_NilReceiverReturnsNil(t *testing.T) {
	t.Parallel()

	var w *Wrapper
	if r := w.Runner(); r != nil {
		t.Errorf("(*Wrapper)(nil).Runner() = %v, want nil — mirrors Manager() nil contract", r)
	}
}

func TestLocalRunner_PlacementIsLocal(t *testing.T) {
	t.Parallel()

	w := NewWrapperLazy("/usr/local/bin/claude", &ClaudeProtocol{}, "claude")
	r := w.Runner()
	if r == nil {
		t.Fatal("Runner() = nil for non-nil wrapper")
	}
	if got := r.Placement(); got != PlacementLocal {
		t.Errorf("Placement() = %q, want %q", got, PlacementLocal)
	}
}

func TestLocalRunner_SpawnDelegatesToWrapper(t *testing.T) {
	t.Parallel()

	// No ShimManager configured: both paths must fail identically with the
	// same sentinel message. This is the behaviour-preservation proof for
	// the extraction — the runner adds nothing and removes nothing.
	w := NewWrapperLazy("/usr/local/bin/claude", &ClaudeProtocol{}, "claude")

	_, directErr := w.Spawn(context.Background(), SpawnOptions{Key: "t"})
	if directErr == nil {
		t.Fatal("wrapper.Spawn without ShimManager: err = nil, want non-nil")
	}

	_, runnerErr := w.Runner().Spawn(context.Background(), SpawnOptions{Key: "t"})
	if runnerErr == nil {
		t.Fatal("runner.Spawn without ShimManager: err = nil, want non-nil")
	}

	if directErr.Error() != runnerErr.Error() {
		t.Errorf("delegation changed error: direct=%q runner=%q", directErr, runnerErr)
	}
	if !strings.Contains(runnerErr.Error(), "shim manager not configured") {
		t.Errorf("runner.Spawn err = %q, want the wrapper's own sentinel", runnerErr)
	}
}
