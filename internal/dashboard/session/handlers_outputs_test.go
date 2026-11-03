package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// GET /api/sessions/outputs lists one session's outputs or every session's,
// newest first, with file roots ready for the file API while the session
// still works where the file was written, and an index rebuilt from the
// journal after a restart lists the same.
func TestHandleOutputs(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	wsDir, otherDir := t.TempDir(), t.TempDir()
	j, err := journal.Open(filepath.Join(state, "log"))
	if err != nil {
		t.Fatal(err)
	}
	reg := workspace.Load(filepath.Join(state, "workspaces.json"), "local")
	ws, _ := reg.Ensure(wsDir)

	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(state, "sessions.json")})
	t.Cleanup(r.Shutdown)
	r.InjectSession("a", nil).SetWorkspaceForTest(wsDir)
	// c has moved on to another tree since it wrote its file.
	r.InjectSession("c", nil).SetWorkspaceForTest(otherDir)

	rec := activity.NewRecorder(j, reg)
	rec.Record("a", wsDir, activity.Activity{Type: activity.TypeOutput, Summary: "docs/r.md", Path: "docs/r.md"})
	rec.Record("a", wsDir, activity.Activity{Type: activity.TypeGitPush, Summary: "git push"})
	rec.Record("b", wsDir, activity.Activity{Type: activity.TypePRCreate, Summary: "gh pr create", URL: "https://github.com/o/r/pull/1", State: activity.StateOpened})
	rec.Record("b", wsDir, activity.Activity{Type: activity.TypePR, Summary: "https://github.com/o/r/pull/1", URL: "https://github.com/o/r/pull/1"})
	rec.Record("c", wsDir, activity.Activity{Type: activity.TypeOutput, Summary: "out.csv", Path: "out.csv"})
	// A merge by number names no PR the index could show.
	rec.Record("b", wsDir, activity.Activity{Type: activity.TypePRMerge, Summary: "gh pr merge 2", State: activity.StateMerged})
	rec.Record("a", wsDir, activity.Activity{Type: activity.TypeOutput, Summary: "docs/r.md", Path: "docs/r.md"})

	type row struct {
		Key, WS, Type, Path, Dir, URL, State, Root string
	}
	list := func(t *testing.T, src OutputSource, query string) []row {
		t.Helper()
		h := New(Deps{Outputs: src, Router: realRouter{r}})
		w := httptest.NewRecorder()
		h.HandleOutputs(w, httptest.NewRequest(http.MethodGet, "/api/sessions/outputs"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", query, w.Code, w.Body)
		}
		var resp struct {
			Outputs []row `json:"outputs"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Outputs
	}
	fileA := row{Key: "a", WS: ws.ID, Type: "output", Path: "docs/r.md", Dir: wsDir, Root: "session:a"}
	fileC := row{Key: "c", WS: ws.ID, Type: "output", Path: "out.csv", Dir: wsDir}
	prB := row{Key: "b", WS: ws.ID, Type: "pr_create", URL: "https://github.com/o/r/pull/1", State: "opened"}

	check := func(t *testing.T, src OutputSource) {
		t.Helper()
		if got := list(t, src, ""); len(got) != 3 || got[0] != fileA || got[1] != fileC || got[2] != prB {
			t.Errorf("all outputs = %+v, want [%+v %+v %+v]", got, fileA, fileC, prB)
		}
		if got := list(t, src, "?key=b"); len(got) != 1 || got[0] != prB {
			t.Errorf("outputs of b = %+v, want [%+v]", got, prB)
		}
	}
	check(t, rec)

	rebuilt := activity.NewRecorder(j, reg)
	if err := rebuilt.Load(time.Now()); err != nil {
		t.Fatal(err)
	}
	check(t, rebuilt)

	if got := list(t, nil, "?key=a"); len(got) != 0 {
		t.Errorf("no index: %+v, want none", got)
	}
	w := httptest.NewRecorder()
	New(Deps{Outputs: rec}).HandleOutputs(w, httptest.NewRequest(http.MethodGet, "/api/sessions/outputs?key=a%00b", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid key: status %d, want 400", w.Code)
	}
}
