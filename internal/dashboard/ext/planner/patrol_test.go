package planner

import (
	"context"
	"encoding/json"
	"errors"
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
		Deny: func() []string { return nil },
		Session: func(ctx context.Context, key string, _ sessionview.AgentOpts, _ string) (string, error) {
			started <- key
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return `{"findings":[]}`, ctx.Err()
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

// decideHandler is a handler over a patrol with two findings on record in
// the workspace gateway, one of them filed as an open item of the ledger
// the handler lists, and a second registered workspace with none. declined
// is where the patrol keeps what the user turned down; reach, set false,
// puts the Planner out of reach.
func decideHandler(t *testing.T) (h *Handler, p *patrol.Patrol, ledger *front.Store, declined string, reach *bool) {
	t.Helper()
	reach = new(bool)
	*reach = true
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := []workspace.Dir{{ID: wsGateway, Name: "gateway", Path: filepath.Join(root, "gateway")}, {ID: wsDocs, Name: "docs", Path: filepath.Join(root, "docs")}}
	for _, d := range dirs {
		if err := os.Mkdir(d.Path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirs[0].Path, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger = front.New(filepath.Join(root, "fronts.json"), nil)
	handed := make(chan struct{}, 1)
	p = patrol.New(patrol.Config{
		Dir:        filepath.Join(root, "planner", "patrol"),
		Workspaces: func() []workspace.Dir { return dirs },
		Items:      ledger,
		Exec: func(context.Context, string, string, ...string) (string, error) {
			return "", os.ErrNotExist
		},
		Deny: func() []string { return nil },
		Session: func(context.Context, string, sessionview.AgentOpts, string) (string, error) {
			return `{"findings":[
				{"id":"bug-one","title":"重连时会话为空会崩","kind":"bug","evidence":["a.go:1","https://example.com/issue/7"],"impact":"重连即崩","fix":"先判空","size":"S","reversible":true,"confidence":"high"},
				{"id":"doc-two","title":"README 与脚本不符","kind":"doc","evidence":["a.go:1"],"impact":"装不上","fix":"改 README","size":"M","reversible":true,"confidence":"medium"}]}`, nil
		},
		// The findings are on record once they are handed over.
		Wake: func(func(int, error), ...string) error { handed <- struct{}{}; return nil },
		// The Planner takes what it is told while reach is true.
		Answer: func(string, string, string) error {
			if !*reach {
				return errors.New("planner is stopped")
			}
			return nil
		},
	})
	t.Cleanup(p.Stop)
	if _, err := p.Trigger(wsGateway); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handed:
	case <-time.After(10 * time.Second):
		t.Fatal("the deep check did not hand its findings over")
	}
	if _, filed, err := p.File(refOne, "", ""); err != nil || !filed {
		t.Fatalf("File: %v, %v", filed, err)
	}
	h = New(nil, nil, nil, nil)
	h.SetPatrol(p)
	h.SetFronts(ledger)
	return h, p, ledger, filepath.Join(root, "planner", "patrol", "declined.json"), reach
}

const (
	wsGateway = "ws_ab12cd34"
	wsDocs    = "ws_ef56ab78"
	refOne    = wsGateway + "/bug-one"
	refTwo    = wsGateway + "/doc-two"
	loopback  = "127.0.0.1:50000"
)

func patrolDecide(h *Handler, body, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/planner/patrol/decide", strings.NewReader(body))
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.HandlePatrolDecide(w, r)
	return w
}

// The user's say on a finding is taken only straight from loopback, only
// in the shape the page sends, and only about a finding that is on record
// in the workspace its ref names (or a workspace this node has): whatever
// is refused writes nothing.
func TestHandlePatrolDecide(t *testing.T) {
	h, p, ledger, declined, reach := decideHandler(t)
	skip := `{"ref":"` + refOne + `","choice":"skip"}`
	for _, tc := range []struct {
		name, body, remote string
		hdr                map[string]string
		code               int
	}{
		{"another machine", skip, "10.0.0.5:50000", nil, http.StatusForbidden},
		{"forwarded", skip, loopback, map[string]string{"X-Forwarded-For": "127.0.0.1"}, http.StatusForbidden},
		{"a reverse node's api call", skip, "", nil, http.StatusForbidden},
		{"not JSON", `ref=` + refOne, loopback, nil, http.StatusBadRequest},
		{"unknown field", `{"ref":"` + refOne + `","choice":"skip","force":true}`, loopback, nil, http.StatusBadRequest},
		{"huge body", `{"ref":"` + strings.Repeat("r", 8<<10) + `","choice":"skip"}`, loopback, nil, http.StatusBadRequest},
		{"no choice", `{"ref":"` + refOne + `"}`, loopback, nil, http.StatusBadRequest},
		{"unknown choice", `{"ref":"` + refOne + `","choice":"merge"}`, loopback, nil, http.StatusBadRequest},
		{"a label for a choice", `{"ref":"` + refOne + `","choice":"不做"}`, loopback, nil, http.StatusBadRequest},
		{"no ref", `{"choice":"skip"}`, loopback, nil, http.StatusBadRequest},
		{"a ref that is none", `{"ref":"bug-one","choice":"skip"}`, loopback, nil, http.StatusBadRequest},
		{"a ref that leaves its workspace", `{"ref":"` + wsGateway + `/../x","choice":"skip"}`, loopback, nil, http.StatusBadRequest},
		{"an upper-case workspace", `{"ref":"WS_AB12CD34/bug-one","choice":"skip"}`, loopback, nil, http.StatusBadRequest},
		{"a workspace beside the ref", `{"ref":"` + refOne + `","workspace":"` + wsGateway + `","choice":"skip"}`, loopback, nil, http.StatusBadRequest},
		{"mute_ws with a ref", `{"ref":"` + refOne + `","workspace":"` + wsGateway + `","choice":"mute_ws"}`, loopback, nil, http.StatusBadRequest},
		{"mute_ws with no workspace", `{"choice":"mute_ws"}`, loopback, nil, http.StatusBadRequest},
		{"mute_ws by name", `{"workspace":"gateway","choice":"mute_ws"}`, loopback, nil, http.StatusBadRequest},
		{"a finding not on record", `{"ref":"` + wsGateway + `/bug-nope","choice":"skip"}`, loopback, nil, http.StatusNotFound},
		{"a finding of another workspace", `{"ref":"` + wsDocs + `/bug-one","choice":"do"}`, loopback, nil, http.StatusNotFound},
		{"a workspace this node does not have", `{"workspace":"ws_zzzzzzz9","choice":"mute_ws"}`, loopback, nil, http.StatusNotFound},
	} {
		if w := patrolDecide(h, tc.body, tc.remote, tc.hdr); w.Code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, w.Code, w.Body, tc.code)
		}
	}
	if _, err := os.Stat(declined); !os.IsNotExist(err) {
		t.Fatalf("refused requests wrote declined.json (%v)", err)
	}
	if r, _ := p.Finding(refOne); r.State != patrol.StateItem || len(ledger.Items()) != 1 {
		t.Fatalf("refused requests changed the record: %+v, %d items", r, len(ledger.Items()))
	}

	// 不做: the finding turned down, its item closed.
	if w := patrolDecide(h, skip, loopback, nil); w.Code != http.StatusOK {
		t.Fatalf("skip: %d %s", w.Code, w.Body)
	}
	if r, _ := p.Finding(refOne); r.State != patrol.StateDeclined || len(ledger.Items()) != 0 {
		t.Errorf("after skip: %+v, %d items", r, len(ledger.Items()))
	}
	// 做 on one turned down: the state does not take it.
	if w := patrolDecide(h, `{"ref":"`+refOne+`","choice":"do"}`, loopback, nil); w.Code != http.StatusConflict {
		t.Errorf("do after skip: %d %s, want 409", w.Code, w.Body)
	}
	// 做 with the Planner out of reach: nothing would start the task, so
	// the page is told and nothing is recorded.
	*reach = false
	if w := patrolDecide(h, `{"ref":"`+refTwo+`","choice":"do"}`, loopback, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), patrol.ErrPlanner.Error()) {
		t.Errorf("do with the Planner stopped: %d %s, want 409", w.Code, w.Body)
	}
	if r, _ := p.Finding(refTwo); r.State == patrol.StateDoing || !r.Disposed.IsZero() || r.Item != "" {
		t.Errorf("after a do that did not go through: %+v", r)
	}
	// 做: handed to the Planner, and shown as an open item until a task
	// has it.
	*reach = true
	if w := patrolDecide(h, `{"ref":"`+refTwo+`","choice":"do"}`, loopback, nil); w.Code != http.StatusOK {
		t.Fatalf("do: %d %s", w.Code, w.Body)
	}
	if r, _ := p.Finding(refTwo); r.State != patrol.StateDoing || len(ledger.Items()) != 1 {
		t.Errorf("after do: %+v, %d items", r, len(ledger.Items()))
	}
	// 这类别再提.
	if w := patrolDecide(h, `{"ref":"`+refTwo+`","choice":"mute_kind"}`, loopback, nil); w.Code != http.StatusOK {
		t.Fatalf("mute_kind: %d %s", w.Code, w.Body)
	}
	// 这个工作区别巡查了: a deep check asked for there is refused from now.
	if w := patrolDecide(h, `{"workspace":"`+wsGateway+`","choice":"mute_ws"}`, loopback, nil); w.Code != http.StatusOK {
		t.Fatalf("mute_ws: %d %s", w.Code, w.Body)
	}
	if w := patrolRun(h, `{"workspace":"`+wsGateway+`"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), patrol.ErrMuted.Error()) {
		t.Errorf("a deep check of a workspace out of the patrol: %d %s", w.Code, w.Body)
	}
	data, err := os.ReadFile(declined)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id": "bug-one"`, `"id": "doc-two"`, `"kind": "doc"`, `"by": "user"`, `"workspace": "` + wsGateway + `"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("declined.json lacks %s:\n%s", want, data)
		}
	}

	// A list that does not read: nothing is taken, nothing written.
	if err := os.WriteFile(declined, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{skip, `{"workspace":"` + wsDocs + `","choice":"mute_ws"}`} {
		if w := patrolDecide(h, body, loopback, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), patrol.ErrDeclined.Error()) {
			t.Errorf("with the list unreadable, %s: %d %s", body, w.Code, w.Body)
		}
	}
	if data, _ := os.ReadFile(declined); string(data) != "{" {
		t.Errorf("the unreadable list was written over: %s", data)
	}
	// No patrol (no Planner state root): off.
	for _, body := range []string{skip, `{"workspace":"` + wsDocs + `","choice":"mute_ws"}`} {
		if w := patrolDecide(New(nil, nil, nil, nil), body, loopback, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), patrol.ErrOff.Error()) {
			t.Errorf("no patrol, %s: %d %s, want 409", body, w.Code, w.Body)
		}
	}
}

// The choice is this machine's: the route is the handler's, and a reverse
// node's `api` call does not reach it.
func TestPatrolDecideNotANodeRoute(t *testing.T) {
	const route = "POST /api/planner/patrol/decide"
	if node.IsAPIRoute(route) {
		t.Errorf("%s is among a reverse node's api routes", route)
	}
	for _, r := range node.APIRoutes() {
		if strings.Contains(r, "/api/planner/patrol") {
			t.Errorf("%s is among a reverse node's api routes", r)
		}
	}
	found := false
	for _, rt := range New(nil, nil, nil, nil).Routes() {
		found = found || rt.Pattern == route
	}
	if !found {
		t.Errorf("%s is not among the handler's routes", route)
	}
}

// The ledger's read carries the findings its open items point at: what the
// page shows under such an item, evidence with where it opens.
func TestHandleFrontsFindings(t *testing.T) {
	h, _, ledger, _, _ := decideHandler(t)
	if _, _, err := ledger.AddItem(front.Item{Text: "手工记的", Workspace: wsGateway, Source: "patrol:" + refTwo, From: front.FromUser}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.HandleFronts(w, frontReq("GET", "", "192.168.1.20:50000", nil))
	var got struct {
		Items    []front.Item             `json:"items"`
		Findings map[string]patrol.Detail `json:"findings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	d, ok := got.Findings[refOne]
	// Only what the patrol filed: an item of the user's naming a finding
	// in its source is the user's.
	if len(got.Items) != 2 || len(got.Findings) != 1 || !ok {
		t.Fatalf("items %+v, findings %+v", got.Items, got.Findings)
	}
	if d.Kind != "bug" || d.Size != "S" || d.Confidence != "high" || d.Impact != "重连即崩" || d.Fix != "先判空" || d.State != patrol.StateItem {
		t.Errorf("finding %+v", d)
	}
	// The link a session wrote as evidence is not shown: the links are the
	// gateway's own, and this directory is no repository on GitHub.
	if len(d.Evidence) != 1 || d.Evidence[0] != (patrol.Evidence{Text: "a.go:1"}) {
		t.Errorf("evidence %+v, want the file line alone", d.Evidence)
	}
	// With no finding among the items the field is left out.
	plain, _ := frontsHandler(t)
	w = httptest.NewRecorder()
	plain.HandleFronts(w, frontReq("GET", "", loopback, nil))
	if strings.Contains(w.Body.String(), "findings") {
		t.Errorf("a ledger with no finding lists findings: %s", w.Body)
	}
}
