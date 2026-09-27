package server

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestDispatchRouter_FailedCreateIsNilInterface: when the router cannot
// produce a session the dispatcher gets a nil interface and the router's
// error, not a typed nil it would hand on to Send.
func TestDispatchRouter_FailedCreateIsNilInterface(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	sess, _, err := (dispatchRouter{r}).GetOrCreate(context.Background(), "feishu:direct:absent:general", sessionview.AgentOpts{})
	if err == nil {
		t.Fatal("GetOrCreate on a router with no CLI wrapper = nil error, want the router's refusal")
	}
	if sess != nil {
		t.Errorf("GetOrCreate failure returned %#v, want a nil interface", sess)
	}
}

// TestDispatchRouter_ALiveSessionComesThrough: an existing session is the one
// the dispatcher gets, and so the one Send later receives.
func TestDispatchRouter_ALiveSessionComesThrough(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	const key = "feishu:direct:present:general"
	proc := session.NewTestProcess()
	want := r.InjectSession(key, proc)
	got, status, err := (dispatchRouter{r}).GetOrCreate(context.Background(), key, sessionview.AgentOpts{})
	if err != nil {
		t.Fatalf("GetOrCreate(present): %v", err)
	}
	if ms, ok := got.(*session.ManagedSession); !ok || ms != want {
		t.Errorf("GetOrCreate(present) = %#v, want the router's session", got)
	}
	if status != sessionview.SessionExisting {
		t.Errorf("status = %v, want SessionExisting", status)
	}
}

type foreignSession struct{}

func (foreignSession) Backend() string { return "claude" }

// TestServerCaps_SendRefusesAForeignSession: Send only accepts the session
// dispatchRouter produced; anything else is reported, not dereferenced.
func TestServerCaps_SendRefusesAForeignSession(t *testing.T) {
	c := serverCaps{s: &Server{}}
	for _, sess := range []dispatch.Session{foreignSession{}, nil} {
		_, err := c.Send(context.Background(), "k", sess, "hi", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "did not produce") {
			t.Errorf("Send(%T) err = %v, want the wiring-fault error", sess, err)
		}
	}
}
