package server

import "testing"

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
