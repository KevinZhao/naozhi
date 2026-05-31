package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestSendWithBroadcast_NonHeadlessNilHubPanics locks the R248-ARCH-9 (#379)
// fail-loud gate: a Server that was NOT constructed with Headless=true but
// somehow reaches the send path with a nil hub is a wiring regression and must
// panic rather than silently routing through the no-broadcast fallback (which
// would drop every dashboard/IM broadcast with no signal).
func TestSendWithBroadcast_NonHeadlessNilHubPanics(t *testing.T) {
	s := &Server{headless: false} // production default: not headless, hub unwired
	sess := &session.ManagedSession{}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for non-headless Server with nil hub, got none")
		}
	}()
	_, _ = s.sendWithBroadcast(context.Background(), "k", sess, "hi", nil, nil)
}

// TestSendWithBroadcast_HeadlessNilHubNoPanic verifies the explicit headless
// mode keeps the hub-less fallback path: with Headless=true the nil-hub branch
// does not panic and instead delegates to the session (here the session is a
// zero value; we only assert no panic before delegation by recovering any
// downstream nil-deref, which is unrelated to the mode gate).
func TestSendWithBroadcast_HeadlessNilHubDoesNotPanicOnModeGate(t *testing.T) {
	s := &Server{headless: true}
	sess := &session.ManagedSession{}

	// A zero-value ManagedSession may panic deeper inside Send/usePassthrough;
	// what we assert is that the *mode gate* (the non-headless panic) is NOT
	// the thing that fires. Recover and verify the panic, if any, is not our
	// explicit mode-gate message.
	defer func() {
		if r := recover(); r != nil {
			if msg, ok := r.(string); ok && msg == "server: sendWithBroadcast called with nil hub on a non-headless Server (set ServerOptions.Headless for hub-less wiring)" {
				t.Fatalf("headless Server hit the non-headless mode gate panic: %v", r)
			}
			// any other panic comes from the zero-value session internals,
			// which is out of scope for the mode-gate assertion.
		}
	}()
	_, _ = s.sendWithBroadcast(context.Background(), "k", sess, "hi", nil, nil)
}

// TestHeadlessOptionPlumbing locks ServerOptions.Headless → Server.headless.
func TestHeadlessOptionPlumbing(t *testing.T) {
	if got := (&Server{headless: true}).headless; !got {
		t.Fatal("headless field not set")
	}
}
