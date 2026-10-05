package wireup

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestUpstreamRouter_AMissingSessionIsANilInterface: the connector checks
// `sess == nil`, so the adapter must turn the router's nil *ManagedSession
// into a nil interface rather than wrapping it.
func TestUpstreamRouter_AMissingSessionIsANilInterface(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	u := UpstreamRouter(r)

	if s := u.SessionFor("feishu:direct:absent:general"); s != nil {
		t.Errorf("SessionFor(absent) = %#v, want a nil interface", s)
	}
	// No CLI wrapper is configured, so ResetAndRecreate fails and hands back
	// no session: that too must be a nil interface.
	s, err := u.ResetAndRecreate(context.Background(), "feishu:direct:nowrapper:general", sessionview.AgentOpts{})
	if err == nil {
		t.Fatal("ResetAndRecreate without a CLI wrapper succeeded (test premise)")
	}
	if s != nil {
		t.Errorf("ResetAndRecreate on failure = %#v, want a nil interface", s)
	}
}

// TestUpstreamRouter_ALiveSessionComesThrough: a session the router has is
// the one the connector gets, so identity comparisons still work.
func TestUpstreamRouter_ALiveSessionComesThrough(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	const key = "feishu:direct:present:general"
	want := r.InjectSession(key, nil)

	got := UpstreamRouter(r).SessionFor(key)
	if got == nil {
		t.Fatal("SessionFor(present) = nil")
	}
	if ms, ok := got.(*session.ManagedSession); !ok || ms != want {
		t.Errorf("SessionFor(present) = %#v, want the router's session", got)
	}
}

// TestUpstreamRouter_TakeoverPrecheckHoldsNothing: the connector's precheck
// reserves the key only to ask, so neither a second precheck nor the
// takeover itself is refused by it.
func TestUpstreamRouter_TakeoverPrecheckHoldsNothing(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	u := UpstreamRouter(r)
	const key = "local:takeover:proj:general"

	for i := range 2 {
		if err := u.TakeoverPrecheck(key); err != nil {
			t.Fatalf("TakeoverPrecheck #%d = %v, want nil", i+1, err)
		}
	}
	// No CLI wrapper is configured, so the takeover gets past every gate and
	// fails in the spawn, then frees the key.
	if _, err := u.Takeover(context.Background(), key, "", t.TempDir(), sessionview.AgentOpts{}); err == nil || errors.Is(err, session.ErrSpawnInFlight) || errors.Is(err, session.ErrMaxProcs) {
		t.Fatalf("Takeover = %v, want a spawn error", err)
	}
	if err := u.TakeoverPrecheck(key); err != nil {
		t.Errorf("TakeoverPrecheck after the takeover = %v, want nil", err)
	}
}
