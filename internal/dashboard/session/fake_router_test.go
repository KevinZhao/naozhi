package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// fakeRouter is a RouterView with no *session.Router behind it. It makes the
// #2561 promise ("interface-typed deps mean tests need not build the real
// object") true for at least one behavioural test in this package (#2635):
// every other dashsession test constructs a real Router.
//
// Only the methods HandleDelete / HandleSetLabel exercise record anything; the
// rest return zero values so the compile-time check below is the only edit
// needed when RouterView grows.
type fakeRouter struct {
	removed   []string
	labels    map[string]string
	knownKeys map[string]bool
}

var _ RouterView = (*fakeRouter)(nil)

func newFakeRouter(keys ...string) *fakeRouter {
	f := &fakeRouter{labels: map[string]string{}, knownKeys: map[string]bool{}}
	for _, k := range keys {
		f.knownKeys[k] = true
	}
	return f
}

func (f *fakeRouter) ListSessionsWithVersion() ([]sessionpkg.SessionSnapshot, uint64) { return nil, 0 }
func (f *fakeRouter) ListSessionsIfChanged(uint64) ([]sessionpkg.SessionSnapshot, uint64, bool) {
	return nil, 0, false
}
func (f *fakeRouter) BumpVersion()                                               {}
func (f *fakeRouter) Stats() (int, int)                                          { return 0, len(f.knownKeys) }
func (f *fakeRouter) SessionFor(string) *sessionpkg.ManagedSession               { return nil }
func (f *fakeRouter) SessionRuns(string, int, time.Time) []runhistory.SessionRun { return nil }
func (f *fakeRouter) SessionRunStats(string) runhistory.SessionRunStats {
	return runhistory.SessionRunStats{}
}
func (f *fakeRouter) SetUserLabel(key, label string) bool {
	if !f.knownKeys[key] {
		return false
	}
	f.labels[key] = label
	return true
}
func (f *fakeRouter) SetSessionTuning(context.Context, string, *string, *string) (string, error) {
	return "", nil
}
func (f *fakeRouter) InterruptSessionSafe(string) sessionpkg.InterruptOutcome {
	var zero sessionpkg.InterruptOutcome
	return zero
}
func (f *fakeRouter) RemoveAsync(key string) bool {
	if !f.knownKeys[key] {
		return false
	}
	delete(f.knownKeys, key)
	f.removed = append(f.removed, key)
	return true
}
func (f *fakeRouter) RegisterForResume(key, _, _, _ string) string { return key }
func (f *fakeRouter) CLIName() string                              { return "fake" }
func (f *fakeRouter) CLIVersion() string                           { return "0" }
func (f *fakeRouter) MaxProcs() int                                { return 1 }
func (f *fakeRouter) DefaultWorkspace() string                     { return "" }
func (f *fakeRouter) Workspace(string) string                      { return "" }
func (f *fakeRouter) DiscoveryExcludeIDs() map[string]bool         { return nil }

// TestHandleDelete_DrivenByFakeRouter: the local-node delete path forwards the
// validated key to RouterView.RemoveAsync and maps its bool to 200 / 404.
func TestHandleDelete_DrivenByFakeRouter(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:alice:general"
	fr := newFakeRouter(key)
	h := New(Deps{Router: fr})

	del := func(k string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/sessions?key="+k, nil)
		w := httptest.NewRecorder()
		h.HandleDelete(w, req)
		return w
	}
	if w := del(key); w.Code != http.StatusOK {
		t.Fatalf("delete known key: status = %d, body %s", w.Code, w.Body.String())
	}
	if len(fr.removed) != 1 || fr.removed[0] != key {
		t.Fatalf("RemoveAsync received %v, want [%s]", fr.removed, key)
	}
	// Second delete of the same key: the view says false → 404.
	if w := del(key); w.Code != http.StatusNotFound {
		t.Fatalf("delete unknown key: status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleSetLabel_DrivenByFakeRouter: the label write reaches the view with
// the validated label, and the response never echoes it.
func TestHandleSetLabel_DrivenByFakeRouter(t *testing.T) {
	t.Parallel()
	const key = "feishu:direct:bob:general"
	fr := newFakeRouter(key)
	h := New(Deps{Router: fr})

	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label",
		strings.NewReader(`{"key":"`+key+`","label":"deploy notes"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleSetLabel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := fr.labels[key]; got != "deploy notes" {
		t.Fatalf("view label = %q, want %q", got, "deploy notes")
	}
	if strings.Contains(w.Body.String(), "deploy notes") {
		t.Fatalf("response echoes the label (reflected-XSS guard): %s", w.Body.String())
	}

	// Unknown key: the view refuses → 404, and nothing is recorded.
	req = httptest.NewRequest(http.MethodPatch, "/api/sessions/label",
		strings.NewReader(`{"key":"feishu:direct:nobody:general","label":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.HandleSetLabel(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown key: status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	if len(fr.labels) != 1 {
		t.Fatalf("labels = %v, want only the first key", fr.labels)
	}
}
