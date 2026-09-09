package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	projectpkg "github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// fakeStore / fakeRouter are ProjectStore / RouterView implementations with
// no *project.Manager or *session.Router behind them (#2635): the first
// dashproject test driven purely through the consumer interfaces this package
// declares, so a handler can be exercised without a projects root on disk.
type fakeStore struct {
	favorites map[string]bool
	known     map[string]bool
}

var _ ProjectStore = (*fakeStore)(nil)

func (f *fakeStore) All() []*projectpkg.Project     { return nil }
func (f *fakeStore) Get(string) *projectpkg.Project { return nil }
func (f *fakeStore) SetFavorite(name string, fav bool) error {
	if !f.known[name] {
		return projectpkg.ErrNotFound
	}
	f.favorites[name] = fav
	return nil
}
func (f *fakeStore) UpdateConfig(string, projectpkg.ProjectConfig) error { return nil }
func (f *fakeStore) EffectivePlannerModel(*projectpkg.Project) string    { return "" }
func (f *fakeStore) EffectivePlannerPrompt(*projectpkg.Project) string   { return "" }

type fakeRouter struct{ bumps int }

var _ RouterView = (*fakeRouter)(nil)

func (f *fakeRouter) SessionFor(string) *session.ManagedSession { return nil }
func (f *fakeRouter) ResetAndRecreate(context.Context, string, session.AgentOpts) (*session.ManagedSession, error) {
	return nil, nil
}
func (f *fakeRouter) BumpVersion() { f.bumps++ }

// TestHandleFavoriteToggle_DrivenByFakes: the favorite flip is forwarded to
// the store, ErrNotFound maps to 404, and every successful write bumps the
// router version so the dashboard's version-gated poll notices.
func TestHandleFavoriteToggle_DrivenByFakes(t *testing.T) {
	t.Parallel()
	store := &fakeStore{favorites: map[string]bool{}, known: map[string]bool{"alpha": true}}
	rt := &fakeRouter{}
	h := New(Deps{ProjectMgr: store, Router: rt})

	toggle := func(name, fav string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/favorite?name="+name+"&favorite="+fav, nil)
		w := httptest.NewRecorder()
		h.HandleFavoriteToggle(w, req)
		return w
	}

	if w := toggle("alpha", "true"); w.Code != http.StatusOK {
		t.Fatalf("favorite=true: status = %d, body %s", w.Code, w.Body.String())
	}
	if !store.favorites["alpha"] {
		t.Fatal("store did not receive SetFavorite(alpha, true)")
	}
	if rt.bumps != 1 {
		t.Fatalf("BumpVersion calls = %d after one write, want 1", rt.bumps)
	}

	if w := toggle("alpha", "false"); w.Code != http.StatusOK {
		t.Fatalf("favorite=false: status = %d, body %s", w.Code, w.Body.String())
	}
	if store.favorites["alpha"] || rt.bumps != 2 {
		t.Fatalf("second write: favorite=%v bumps=%d, want false / 2", store.favorites["alpha"], rt.bumps)
	}

	if w := toggle("ghost", "true"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown project: status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	if rt.bumps != 2 {
		t.Fatalf("a failed write must not bump the version; bumps = %d", rt.bumps)
	}
	if w := toggle("alpha", "maybe"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad favorite value: status = %d, want 400", w.Code)
	}
}
