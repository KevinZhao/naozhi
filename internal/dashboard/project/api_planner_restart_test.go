package project

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	projectpkg "github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

type stubPlannerResolver struct {
	key  string
	opts sessionview.AgentOpts
	ok   bool
}

func (s stubPlannerResolver) ResolveForPlannerKey(string) (string, sessionview.AgentOpts, bool) {
	return s.key, s.opts, s.ok
}

func restartPlanner(h *Handlers, name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/projects/planner/restart?name="+name, nil)
	w := httptest.NewRecorder()
	h.HandlePlannerRestart(w, req)
	return w
}

// TestHandlePlannerRestart_ResolverIsTheOnlyOptsSource: planner restart spawns
// with exactly the resolver's key and opts, and without a resolver it refuses
// even when the store knows the project (#3300).
func TestHandlePlannerRestart_ResolverIsTheOnlyOptsSource(t *testing.T) {
	t.Parallel()
	store := &fakeStore{projects: map[string]*projectpkg.Project{
		"alpha": {Name: "alpha", Path: "/tmp/alpha"},
	}}

	t.Run("nil resolver refuses", func(t *testing.T) {
		t.Parallel()
		rt := &fakeRouter{}
		w := restartPlanner(New(Deps{ProjectMgr: store, Router: rt}), "alpha")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "projects not configured") {
			t.Fatalf("status = %d body %q, want 400 projects not configured", w.Code, w.Body.String())
		}
		if rt.resets != 0 {
			t.Fatalf("ResetAndRecreate called %d times without a resolver", rt.resets)
		}
	})

	t.Run("resolver opts pass through", func(t *testing.T) {
		t.Parallel()
		want := sessionview.AgentOpts{
			Exempt: true, Workspace: "/tmp/alpha", Model: "opus",
			Backend: "kiro", AccessProfile: "1p", SystemPrompt: "plan",
		}
		rt := &fakeRouter{}
		res := stubPlannerResolver{key: "project:alpha:planner", opts: want, ok: true}
		w := restartPlanner(New(Deps{ProjectMgr: store, Router: rt, Resolver: res}), "alpha")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body %q, want 200", w.Code, w.Body.String())
		}
		if rt.resets != 1 || rt.resetKey != res.key || !reflect.DeepEqual(rt.resetOpts, want) {
			t.Fatalf("ResetAndRecreate(%d calls, key %q, opts %+v), want 1 call, %q, %+v",
				rt.resets, rt.resetKey, rt.resetOpts, res.key, want)
		}
	})

	t.Run("resolver miss is 404", func(t *testing.T) {
		t.Parallel()
		rt := &fakeRouter{}
		w := restartPlanner(New(Deps{ProjectMgr: store, Router: rt, Resolver: stubPlannerResolver{}}), "alpha")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d body %q, want 404", w.Code, w.Body.String())
		}
		if rt.resets != 0 {
			t.Fatalf("ResetAndRecreate called %d times on a resolver miss", rt.resets)
		}
	})
}
