package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestSessionHandlers_NilDepsStayNilInterfaces pins the typed-nil hazard that
// #2561 walked into. dashsession's Deps.Router / ProjectMgr / RetiredStore
// became consumer-side interfaces, and dashsession nil-guards them (projectMgr
// ×3, retiredStore ×5, router ×1) because each is optional: no ProjectManager
// when projects are unconfigured, no RetiredStore without a StateDir.
//
// Assigning a nil *project.Manager straight into an interface field makes
// `h.projectMgr != nil` read TRUE, so the guard passes and the next call
// dereferences nil. That is not hypothetical — the first attempt at this
// conversion panicked in Manager.ResolveWorkspaces through WarmHistoryCache,
// inside a singleflight call that turned it into a confusing wrapped panic.
// Same class as the MessageEnqueuer hazard in #377 and the sendEngineOpts.Queue
// one in #2551: an interface-typed field cannot be handed a nil concrete
// pointer.
//
// A Server built with none of the three optional deps must therefore leave all
// three interface fields nil, so the guards inside dashsession still work.
func TestSessionHandlers_NilDepsStayNilInterfaces(t *testing.T) {
	t.Parallel()

	// No ProjectManager, no StateDir (⇒ no RetiredStore) — the shape a minimal
	// deployment and most tests use.
	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: session.NewRouter(session.RouterConfig{}),
	})
	t.Cleanup(srv.appCancel)

	if srv.sessionH == nil {
		t.Fatal("sessionH not wired")
	}

	// The invariant, stated generically: an interface-typed dep may be nil, or it
	// may hold a usable value — but never a non-nil interface wrapping a nil
	// pointer, because that is what defeats the `!= nil` guards.
	//
	// Checked with reflection rather than `== nil` per field, since `== nil` is
	// precisely the comparison the hazard fools. Note RetiredStore is always
	// present: NewRetiredStore("") returns a real store that degrades to a no-op,
	// which is why the first version of this test asserted the wrong thing.
	for name, dep := range map[string]any{
		"ProjectMgr":   srv.sessionH.ProjectSourceForTest(),
		"RetiredStore": srv.sessionH.RetiredStoreForTest(),
	} {
		if dep == nil {
			continue // legitimately unwired
		}
		v := reflect.ValueOf(dep)
		if v.Kind() == reflect.Ptr && v.IsNil() {
			t.Errorf("%s is a non-nil interface (%T) wrapping a nil pointer — every "+
				"`h.%s != nil` guard in dashsession now passes and dereferences nil",
				name, dep, strings.ToLower(name[:1])+name[1:])
		}
	}

	// The path that actually panicked: history warm-up resolves workspaces
	// through projectMgr. It must be a no-op, not a nil deref.
	srv.sessionH.WarmHistoryCache()
	srv.sessionH.WaitWarmHistory()
}
