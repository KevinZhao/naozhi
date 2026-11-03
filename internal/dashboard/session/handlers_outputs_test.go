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
)

// GET /api/sessions/outputs lists one session's outputs or every session's,
// newest first, with file roots ready for the file API, and an index rebuilt
// from the journal after a restart lists the same.
func TestHandleOutputs(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	wsDir := t.TempDir()
	j, err := journal.Open(filepath.Join(state, "log"))
	if err != nil {
		t.Fatal(err)
	}
	reg := workspace.Load(filepath.Join(state, "workspaces.json"), "local")
	ws, _ := reg.Ensure(wsDir)

	rec := activity.NewRecorder(j, reg)
	rec.Record("a", wsDir, activity.Activity{Type: activity.TypeOutput, Summary: "docs/r.md", Path: "docs/r.md"})
	rec.Record("a", wsDir, activity.Activity{Type: activity.TypeGitPush, Summary: "git push"})
	rec.Record("b", wsDir, activity.Activity{Type: activity.TypePRCreate, Summary: "gh pr create", URL: "https://github.com/o/r/pull/1", State: activity.StateOpened})
	rec.Record("b", wsDir, activity.Activity{Type: activity.TypePR, Summary: "https://github.com/o/r/pull/1", URL: "https://github.com/o/r/pull/1"})
	rec.Record("a", wsDir, activity.Activity{Type: activity.TypeOutput, Summary: "docs/r.md", Path: "docs/r.md"})

	type row struct {
		Key, WS, Type, Path, URL, State, Root string
	}
	list := func(t *testing.T, src OutputSource, query string) []row {
		t.Helper()
		h := New(Deps{Outputs: src})
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
	fileA := row{Key: "a", WS: ws.ID, Type: "output", Path: "docs/r.md", Root: "session:a"}
	prB := row{Key: "b", WS: ws.ID, Type: "pr_create", URL: "https://github.com/o/r/pull/1", State: "opened"}

	check := func(t *testing.T, src OutputSource) {
		t.Helper()
		if got := list(t, src, ""); len(got) != 2 || got[0] != fileA || got[1] != prB {
			t.Errorf("all outputs = %+v, want [%+v %+v]", got, fileA, prB)
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
