package server

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestProjectRouter_MissingSessionIsNilInterface: a key the router does not
// have reaches the project package as a nil interface, not a typed nil.
func TestProjectRouter_MissingSessionIsNilInterface(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	if got := (projectRouter{r}).SessionFor("dashboard:direct:absent:general"); got != nil {
		t.Errorf("SessionFor(absent) = %#v, want a nil interface", got)
	}
}

// TestProjectRouter_ALiveSessionComesThrough: a session the router has is
// the one the project package reads.
func TestProjectRouter_ALiveSessionComesThrough(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	const key = "dashboard:direct:present:general"
	want := r.InjectSession(key, nil)
	got := (projectRouter{r}).SessionFor(key)
	if ms, ok := got.(*session.ManagedSession); !ok || ms != want {
		t.Errorf("SessionFor(present) = %#v, want the router's session", got)
	}
}

// TestProjectRouter_RecreateFailureComesThrough: a restart the router refuses
// is reported as a failure, not as a restarted planner.
func TestProjectRouter_RecreateFailureComesThrough(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	err := (projectRouter{r}).ResetAndRecreate(context.Background(), "dashboard:direct:absent:general", sessionview.AgentOpts{})
	if err == nil {
		t.Fatal("ResetAndRecreate on a router with no CLI wrapper = nil error, want the router's refusal")
	}
}
