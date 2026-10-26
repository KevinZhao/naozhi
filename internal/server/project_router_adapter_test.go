package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
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
