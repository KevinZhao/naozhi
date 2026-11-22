package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/patrol"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// patrolHandler is a handler over a patrol whose deep check session waits
// on hold, in a state root with one workspace named gateway.
func patrolHandler(t *testing.T) (h *Handler, hold chan struct{}, started chan string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "gateway")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hold, started = make(chan struct{}), make(chan string, 4)
	p := patrol.New(patrol.Config{
		Dir:        filepath.Join(root, "planner", "patrol"),
		Workspaces: func() []workspace.Dir { return []workspace.Dir{{ID: "ws_ab12cd34", Name: "gateway", Path: dir}} },
		Items:      front.New("", nil),
		// No git here: the directory reads as no repository.
		Exec: func(context.Context, string, string, ...string) (string, error) {
			return "", os.ErrNotExist
		},
		Session: func(ctx context.Context, key string, _ sessionview.AgentOpts, _ string) (string, []string, []string, error) {
			started <- key
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return `{"findings":[]}`, nil, nil, ctx.Err()
		},
	})
	t.Cleanup(p.Stop)
	h = New(nil, nil, nil, nil)
	h.SetPatrol(p)
	return h, hold, started
}

func patrolRun(h *Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.HandlePatrolRun(w, httptest.NewRequest("POST", "/api/planner/patrol/run", strings.NewReader(body)))
	return w
}

// A deep check on demand: 202 with the run's ID for a workspace this node
// has, by ID or name; 409 while one runs; 404 for a workspace it has not;
// 400 for a body that names none.
func TestHandlePatrolRun(t *testing.T) {
	h, hold, started := patrolHandler(t)
	for name, body := range map[string]string{
		"not JSON":      `workspace=gateway`,
		"no workspace":  `{}`,
		"empty":         `{"workspace":""}`,
		"unknown field": `{"workspace":"gateway","force":true}`,
		"too long":      `{"workspace":"` + strings.Repeat("w", 200) + `"}`,
		"huge body":     `{"workspace":"` + strings.Repeat("w", 8<<10) + `"}`,
	} {
		if w := patrolRun(h, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body)
		}
	}
	for _, ref := range []string{"ws_zzzzzzz9", "docs", "../gateway"} {
		if w := patrolRun(h, `{"workspace":"`+ref+`"}`); w.Code != http.StatusNotFound {
			t.Errorf("workspace %q: %d %s, want 404", ref, w.Code, w.Body)
		}
	}
	w := patrolRun(h, `{"workspace":"Gateway"}`)
	var got struct{ Status, Run string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusAccepted || got.Status != "accepted" || got.Run == "" {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	select {
	case key := <-started:
		if key != "patrol:"+got.Run {
			t.Errorf("session %q, want the run %s under the one-shot key", key, got.Run)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the deep check did not start")
	}
	if w := patrolRun(h, `{"workspace":"ws_ab12cd34"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), patrol.ErrBusy.Error()) {
		t.Errorf("while one runs: %d %s, want 409", w.Code, w.Body)
	}
	close(hold)

	// No patrol (no Planner state root): off.
	if w := patrolRun(New(nil, nil, nil, nil), `{"workspace":"gateway"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), patrol.ErrOff.Error()) {
		t.Errorf("no patrol: %d %s, want 409", w.Code, w.Body)
	}
}

// The run is this machine's: a reverse node's `api` call does not reach it,
// as it does not reach the scout's.
func TestPatrolRunNotANodeRoute(t *testing.T) {
	for _, route := range []string{"POST /api/planner/patrol/run", "POST /api/scout/run"} {
		if node.IsAPIRoute(route) {
			t.Errorf("%s is among a reverse node's api routes", route)
		}
	}
	h := New(nil, nil, nil, nil)
	found := false
	for _, rt := range h.Routes() {
		found = found || rt.Pattern == "POST /api/planner/patrol/run"
	}
	if !found {
		t.Error("POST /api/planner/patrol/run is not among the handler's routes")
	}
}
