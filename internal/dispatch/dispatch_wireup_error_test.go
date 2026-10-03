package dispatch

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestNewDispatcher_MissingTurnsReturnsError pins R250-ARCH-12 for the turn
// wiring: a dispatcher without Turns would fail on its first IM message,
// after the healthcheck has passed, so NewDispatcher refuses it with
// ErrTurnsWireupMissing — for a nil Turns and for a nil *turn.Orchestrator
// boxed into one — and accepts any real one.
func TestNewDispatcher_MissingTurnsReturnsError(t *testing.T) {
	t.Parallel()
	for name, turns := range map[string]Turns{"nil": nil, "typed nil": (*turn.Orchestrator)(nil)} {
		d, err := NewDispatcher(DispatcherConfig{Turns: turns})
		if !errors.Is(err, ErrTurnsWireupMissing) {
			t.Errorf("%s Turns: err = %v, want ErrTurnsWireupMissing", name, err)
		}
		if d != nil {
			t.Errorf("%s Turns: dispatcher = %v, want nil on error", name, d)
		}
	}
	d, err := NewDispatcher(DispatcherConfig{
		Turns:  testTurns(),
		Agents: map[string]session.AgentOpts{"general": {}},
	})
	if err != nil || d == nil {
		t.Fatalf("NewDispatcher with Turns = (%v, %v), want a dispatcher", d, err)
	}
}
