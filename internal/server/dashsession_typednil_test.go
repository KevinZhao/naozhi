package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
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
func TestDashboardDeps_NilStayNilInterfaces(t *testing.T) {
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
	deps := map[string]any{
		"dashsession.ProjectMgr":   srv.sessionH.ProjectSourceForTest(),
		"dashsession.RetiredStore": srv.sessionH.RetiredStoreForTest(),
	}
	// dashproject carries the largest guard count of the three (projectMgr ×9,
	// router ×2, resolver ×1), so it is checked here too rather than trusted.
	_, hs := buildServerWithHandlers(ServerOptions{
		Addr:   ":0",
		Router: session.NewRouter(session.RouterConfig{}),
	})
	if hs.projectH == nil {
		t.Fatal("projectH not wired")
	}
	for k, v := range hs.projectH.DepsForTest() {
		deps[k] = v
	}

	for name, dep := range deps {
		if dep == nil {
			continue // legitimately unwired
		}
		v := reflect.ValueOf(dep)
		if v.Kind() == reflect.Ptr && v.IsNil() {
			t.Errorf("%s is a non-nil interface (%T) wrapping a nil pointer — every "+
				"`!= nil` guard on it now passes and dereferences nil", name, dep)
		}
	}

	// The path that actually panicked: history warm-up resolves workspaces
	// through projectMgr. It must be a no-op, not a nil deref.
	srv.sessionH.WarmHistoryCache()
	srv.sessionH.WaitWarmHistory()
}

// TestCronHandlers_NilSchedulerStaysNilInterface is the dashcron half. A nil
// Scheduler is the documented "cron disabled" state — the /cron endpoints answer
// 404/400 rather than panicking — and dashcron nil-guards h.scheduler in 16
// places. Boxing a nil *cron.Scheduler into SchedulerView would defeat all 16 at
// once, and the symptom would be a panic on the first /api/cron request of a
// deployment that never configured cron.
func TestCronHandlers_NilSchedulerStaysNilInterface(t *testing.T) {
	t.Parallel()

	// No Scheduler: the shape of any deployment with cron off.
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:   ":0",
		Router: session.NewRouter(session.RouterConfig{}),
	})
	t.Cleanup(srv.appCancel)

	if hs.cronH == nil {
		t.Fatal("cronH not wired")
	}
	if got := hs.cronH.SchedulerForTest(); got != nil {
		v := reflect.ValueOf(got)
		if v.Kind() == reflect.Ptr && v.IsNil() {
			t.Fatalf("Scheduler is a non-nil interface (%T) wrapping a nil pointer — all 16 "+
				"`h.scheduler != nil` guards in dashcron are defeated", got)
		}
	}

	// The behavioural half: a cron read must answer, not panic.
	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/cron", nil)
	req.Host = "naozhi.example"
	w := httptest.NewRecorder()
	hs.cronH.HandleList(w, req)
	if w.Code == 0 {
		t.Error("HandleList wrote no status with cron disabled")
	}
}
