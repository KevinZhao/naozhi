package server

import (
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/session"
)

// TestNewHub_NilQueue_LeavesInterfaceFieldNil pins the typed-nil guard for
// the R242-GO-10 (#377) change that turned Hub.queue from a concrete
// *dispatch.MessageQueue into the MessageEnqueuer interface. Assigning a
// nil concrete pointer straight into an interface field would make
// `h.queue == nil` read false and silently disable the legacy-fallback
// gate in send.go. NewHub must only assign a non-nil Queue, so a Hub built
// without a queue keeps a nil interface field.
func TestNewHub_NilQueue_LeavesInterfaceFieldNil(t *testing.T) {
	hub, _ := newTestHub("") // newTestHub wires no Queue
	t.Cleanup(hub.Shutdown)
	if hub.queue != nil {
		t.Fatalf("Hub built without Queue: h.queue = %v, want nil interface", hub.queue)
	}

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	var nodesMu sync.RWMutex
	q := dispatch.NewMessageQueueWithMode(5, 0, dispatch.ModeCollect)
	withQueue := NewHub(HubOptions{Router: router, Guard: guard, NodesMu: &nodesMu, Queue: q})
	t.Cleanup(withQueue.Shutdown)
	if withQueue.queue == nil {
		t.Fatal("Hub built with a real Queue: h.queue is nil, want non-nil interface")
	}
}

// TestLegacySendInvokes_AtomicCounter pins the R-LEGACY-SEND (#710) hook.
// LegacySendInvokes() is the migration handle: production Hubs wire a
// real MessageQueue and never bump the counter; tests that omit Queue
// fall through `if h.queue == nil { sessionSendLegacy }` in send.go and
// the counter advances. Once every test fixture wires a queue stub, the
// counter stays at zero and sessionSendLegacy can be deleted alongside
// its sole caller branch.
func TestLegacySendInvokes_AtomicCounter(t *testing.T) {
	// nil receiver: defensive — package callers may probe a not-yet-built
	// Hub via interface; the helper documents this returns 0 instead of
	// panicking. R-LEGACY-SEND tooling depends on this.
	var hNil *Hub
	if got := hNil.LegacySendInvokes(); got != 0 {
		t.Fatalf("nil Hub LegacySendInvokes = %d, want 0", got)
	}

	h := &Hub{}
	if got := h.LegacySendInvokes(); got != 0 {
		t.Fatalf("fresh Hub LegacySendInvokes = %d, want 0", got)
	}
	h.legacySendInvokes.Add(1)
	h.legacySendInvokes.Add(1)
	h.legacySendInvokes.Add(1)
	if got := h.LegacySendInvokes(); got != 3 {
		t.Errorf("after 3 bumps LegacySendInvokes = %d, want 3", got)
	}
}
