package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// GET /api/sessions/detail names a session's registered workspace and health
// unit, leaving out what is unknown.
func TestHandleDetail(t *testing.T) {
	t.Parallel()
	state, wsDir := t.TempDir(), t.TempDir()
	reg := workspace.Load(filepath.Join(state, "workspaces.json"), "local")
	ws, _ := reg.Ensure(wsDir)
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(state, "sessions.json")})
	t.Cleanup(r.Shutdown)
	r.InjectSession("a", nil).SetWorkspaceForTest(wsDir)
	r.InjectSession("b", nil).SetWorkspaceForTest(t.TempDir())
	snap := health.Snapshot{Units: []health.Unit{{ID: "a", State: health.Stalled, Evidence: "no output for 61m"}}}

	tests := []struct {
		name, query string
		deps        Deps
		code        int
		want        sessionDetail
	}{
		{"registered and judged", "?key=a", Deps{Router: realRouter{r}, Workspaces: reg, Health: func() health.Snapshot { return snap }}, http.StatusOK,
			sessionDetail{Key: "a", Workspace: &workspaceRef{ID: ws.ID, Name: ws.Name}, Health: &snap.Units[0]}},
		{"outside every workspace, not judged yet", "?key=b", Deps{Router: realRouter{r}, Workspaces: reg, Health: func() health.Snapshot { return snap }}, http.StatusOK,
			sessionDetail{Key: "b"}},
		{"no planner state", "?key=a", Deps{Router: realRouter{r}}, http.StatusOK, sessionDetail{Key: "a"}},
		{"no key", "", Deps{Router: realRouter{r}}, http.StatusBadRequest, sessionDetail{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			New(tc.deps).HandleDetail(w, httptest.NewRequest(http.MethodGet, "/api/sessions/detail"+tc.query, nil))
			if w.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.code, w.Body)
			}
			if tc.code != http.StatusOK {
				return
			}
			var got sessionDetail
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(tc.want)
			if have, _ := json.Marshal(got); string(have) != string(want) {
				t.Errorf("detail = %s, want %s", have, want)
			}
		})
	}
}
