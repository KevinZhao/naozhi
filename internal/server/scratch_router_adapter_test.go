package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestScratchRouter_MissingSourceIsNilInterface: a quoted key the router does
// not have reaches the scratch handler as a nil interface, so it answers 404
// instead of dereferencing a typed nil.
func TestScratchRouter_MissingSourceIsNilInterface(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	if got := (scratchRouter{r}).SessionFor("dashboard:direct:absent:general"); got != nil {
		t.Errorf("SessionFor(absent) = %#v, want a nil interface", got)
	}
}

// TestScratchRouter_ALiveSourceComesThrough: an existing session is the one
// the handler reads the quote context from.
func TestScratchRouter_ALiveSourceComesThrough(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	const key = "dashboard:direct:present:general"
	want := r.InjectSession(key, nil)
	if ms, ok := (scratchRouter{r}).SessionFor(key).(*session.ManagedSession); !ok || ms != want {
		t.Errorf("SessionFor(present) did not return the router's session")
	}
}
