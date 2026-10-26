package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/session"
)

// TestNewDispatcher_NilProjectMgrStaysUntypedNil pins the typed-nil fix
// for ProjectStore (ARCH-DISP-1, #457): cfg.ProjectMgr is a concrete
// *project.Manager, so a nil value boxed into the interface field would
// be != nil and defeat every `d.projectMgr == nil` gate. NewDispatcher
// must collapse it to untyped nil.
func TestNewDispatcher_NilProjectMgrStaysUntypedNil(t *testing.T) {
	t.Parallel()
	d, err := NewDispatcher(DispatcherConfig{ProjectMgr: nil, Turns: testTurns()})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if d.projectMgr != nil {
		t.Fatal("Dispatcher.projectMgr should be untyped nil when cfg.ProjectMgr is nil; typed-nil trap reintroduced")
	}
}

// fakeSessionRouter is a minimal SessionRouter implementation for
// Dispatcher tests. Methods marked "not configured" panic so a test
// that accidentally exercises an unexpected code path surfaces
// immediately rather than silently returning zero values.
type fakeSessionRouter struct {
	interruptViaControl func(key string) session.InterruptOutcome
}

func (f *fakeSessionRouter) ResetChatAndSetWorkspace(string, string) {
	panic("fakeSessionRouter.ResetChatAndSetWorkspace not configured")
}

func (f *fakeSessionRouter) Workspace(string) string {
	panic("fakeSessionRouter.Workspace not configured")
}

func (f *fakeSessionRouter) SetSessionTuning(context.Context, string, *string, *string) (string, error) {
	panic("fakeSessionRouter.SetSessionTuning not configured")
}

func (f *fakeSessionRouter) SetSessionBackend(string, string) {
	panic("fakeSessionRouter.SetSessionBackend not configured")
}

func (f *fakeSessionRouter) VisitSessions(func(session.SessionSnapshot) bool) {
	panic("fakeSessionRouter.VisitSessions not configured")
}

func (f *fakeSessionRouter) InterruptSessionViaControl(key string) session.InterruptOutcome {
	if f.interruptViaControl == nil {
		panic("fakeSessionRouter.InterruptSessionViaControl not configured")
	}
	return f.interruptViaControl(key)
}

// TestDispatcher_ResetRoutesThroughTurns proves /new reaches the session's
// in-flight passthrough sends and the session reset through Turns (the
// orchestrator's Sender), never the session itself (#1612): the Sender
// observes (key, ErrSessionReset) and then the reset of that key.
func TestDispatcher_ResetRoutesThroughTurns(t *testing.T) {
	t.Parallel()

	var calls []string
	sender := &testSender{
		discardPending: func(key string, reason error) {
			if !errors.Is(reason, clierr.ErrSessionReset) {
				t.Errorf("DiscardPending reason = %v, want ErrSessionReset", reason)
			}
			calls = append(calls, "discard "+key)
		},
		reset: func(key string, discardOverride bool) {
			calls = append(calls, fmt.Sprintf("reset %s %v", key, discardOverride))
		},
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSender(sender))
	d.BuildHandler()(context.Background(), incomingMsg("/new"))

	want := []string{"discard fake:direct:chat1:general", "reset fake:direct:chat1:general false"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("Sender calls = %q, want %q (discard before reset, override kept)", calls, want)
	}
}

// TestNewDispatcher_NilRouterStaysUntypedNil pins the typed-nil fix:
// when DispatcherConfig.Router is nil, or a nil *session.Router boxed into
// it, the Dispatcher.router interface field must hold untyped nil so
// `if d.router != nil` guards behave correctly.
func TestNewDispatcher_NilRouterStaysUntypedNil(t *testing.T) {
	t.Parallel()
	for _, r := range []SessionRouter{nil, (*session.Router)(nil)} {
		d, err := NewDispatcher(DispatcherConfig{Router: r, Turns: testTurns()})
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		if d.router != nil {
			t.Fatalf("Dispatcher.router should be untyped nil when cfg.Router is %#v; typed-nil trap reintroduced", r)
		}
	}
}
