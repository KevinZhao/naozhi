package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// TestNewHub_NilQueue_LeavesInterfaceFieldNil pins the typed-nil guard for
// the R242-GO-10 (#377) change that turned Hub.queue from a concrete
// *turn.Queue into the MessageEnqueuer interface (the field now
// lives on sendEngine, #2551). Assigning a
// nil concrete pointer straight into an interface field would make
// `e.queue == nil` read false and silently disable the legacy-fallback
// gate in send.go. newSendEngine must only assign a non-nil Queue, so an
// engine built without a queue keeps a nil interface field. newHubForTest
// always wires a queue, so the nil half builds the engine directly.
func TestNewHub_NilQueue_LeavesInterfaceFieldNil(t *testing.T) {
	noQueue := newSendEngine(sendEngineOpts{})
	t.Cleanup(noQueue.drain)
	if noQueue.queue != nil {
		t.Fatalf("engine built without Queue: queue = %v, want nil interface", noQueue.queue)
	}

	q := turn.NewQueueWithMode(5, 0, turn.ModeCollect)
	withQueue := newHubForTest(t, HubOptions{Router: session.NewRouter(session.RouterConfig{})}, sendEngineOpts{Queue: q})
	t.Cleanup(withQueue.Shutdown)
	if withQueue.engine.queue != MessageEnqueuer(q) {
		t.Fatalf("Hub built with a real Queue: engine.queue = %v, want that queue", withQueue.engine.queue)
	}
}

// TestLegacySendInvokes_AtomicCounter pins the R-LEGACY-SEND (#710) hook.
// LegacySendInvokes() is the migration handle: production Hubs wire a
// real turn.Queue and never bump the counter; tests that omit Queue
// fall through `if e.queue == nil { sessionSendLegacy }` in send.go and
// the counter advances. Once every test fixture wires a queue stub, the
// counter stays at zero and sessionSendLegacy can be deleted alongside
// its sole caller branch.
func TestLegacySendInvokes_AtomicCounter(t *testing.T) {
	// nil receiver: defensive — package callers may probe a not-yet-built
	// engine via interface; the helper documents this returns 0 instead of
	// panicking. R-LEGACY-SEND tooling depends on this.
	var eNil *sendEngine
	if got := eNil.LegacySendInvokes(); got != 0 {
		t.Fatalf("nil engine LegacySendInvokes = %d, want 0", got)
	}

	e := newSendEngine(sendEngineOpts{})
	if got := e.LegacySendInvokes(); got != 0 {
		t.Fatalf("fresh engine LegacySendInvokes = %d, want 0", got)
	}
	e.legacyInvokes.Add(1)
	e.legacyInvokes.Add(1)
	e.legacyInvokes.Add(1)
	if got := e.LegacySendInvokes(); got != 3 {
		t.Errorf("after 3 bumps LegacySendInvokes = %d, want 3", got)
	}
}

// TestLegacySendInvokes_SessionSendAdvances is the positive control for the
// newHubForTest cleanup gate: that gate reads a counter no helper-built test
// can advance any more, so this drives one real sessionSend through a
// queue-less engine and proves the counter still fires. The engine is drained
// first so sessionSendLegacy returns busy without starting a turn. Delete it
// with sessionSendLegacy.
func TestLegacySendInvokes_SessionSendAdvances(t *testing.T) {
	e := newSendEngine(sendEngineOpts{
		Router: session.NewRouter(session.RouterConfig{}),
		Guard:  session.NewGuard(),
	})
	e.drain()
	if _, status, err := e.sessionSend(sendParams{Key: "dashboard:direct:legacy:general", Text: "hi"}, nil); err != nil || status != sendAckBusy {
		t.Fatalf("sessionSend on a drained queue-less engine = (%q, %v), want (%q, nil)", status, err, sendAckBusy)
	}
	if got := e.LegacySendInvokes(); got != 1 {
		t.Fatalf("LegacySendInvokes after one queue-less sessionSend = %d, want 1", got)
	}
}
