package planner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/planner/front"
)

func strp(s string) *string { return &s }

func frontsHandler(t *testing.T) (*Handler, *front.Store) {
	t.Helper()
	s := front.New(filepath.Join(t.TempDir(), "fronts.json"), nil)
	if _, err := s.Apply(front.Update{ID: "jd-tgw", Name: "JD TGW 迁移", State: strp("方案被否"), Waiting: strp("客户")}); err != nil {
		t.Fatal(err)
	}
	h := New(nil, nil, nil, nil)
	h.SetFronts(s)
	return h, s
}

// req is a request as it reaches the listener from remote, with hdr set.
func frontReq(method, body, remote string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(method, "/api/planner/fronts", strings.NewReader(body))
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// The ledger lists to any caller past the page's auth and says whether that
// caller may edit it: only one straight from loopback (D134).
func TestHandleFronts(t *testing.T) {
	h, _ := frontsHandler(t)
	for _, tc := range []struct {
		name, remote string
		hdr          map[string]string
		writable     bool
	}{
		{"loopback", "127.0.0.1:50000", nil, true},
		{"another machine", "192.168.1.20:50000", nil, false},
		{"through a proxy on this machine", "127.0.0.1:50000", map[string]string{"X-Forwarded-For": "1.2.3.4"}, false},
	} {
		w := httptest.NewRecorder()
		h.HandleFronts(w, frontReq("GET", "", tc.remote, tc.hdr))
		var got frontsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if len(got.Fronts) != 1 || got.Fronts[0].ID != "jd-tgw" || got.Writable != tc.writable {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
	w := httptest.NewRecorder()
	New(nil, nil, nil, nil).HandleFronts(w, frontReq("GET", "", "127.0.0.1:1", nil))
	if got := strings.TrimSpace(w.Body.String()); got != `{"fronts":[],"items":[],"writable":false}` {
		t.Errorf("no ledger: %s", got)
	}
}

// An edit is taken only straight from loopback, changes the fields it
// names, and never makes a line: the page edits what it lists.
func TestHandleFrontEdit(t *testing.T) {
	const local = "127.0.0.1:50000"
	for _, tc := range []struct {
		name, body, remote string
		hdr                map[string]string
		code               int
	}{
		{"another machine", `{"id":"jd-tgw","close":true}`, "10.0.0.5:50000", nil, http.StatusForbidden},
		{"forwarded", `{"id":"jd-tgw","close":true}`, local, map[string]string{"X-Forwarded-For": "127.0.0.1"}, http.StatusForbidden},
		{"a reverse node's api call", `{"id":"jd-tgw","close":true}`, "", nil, http.StatusForbidden},
		{"unknown line", `{"id":"nope","name":"新的"}`, local, nil, http.StatusNotFound},
		{"bad id", `{"id":"JD TGW"}`, local, nil, http.StatusBadRequest},
		{"unknown field", `{"id":"jd-tgw","sessions":["k"]}`, local, nil, http.StatusBadRequest},
		{"not JSON", `close it`, local, nil, http.StatusBadRequest},
	} {
		h, s := frontsHandler(t)
		w := httptest.NewRecorder()
		h.HandleFrontEdit(w, frontReq("POST", tc.body, tc.remote, tc.hdr))
		if w.Code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, w.Code, w.Body, tc.code)
		}
		if open := s.Open(); len(open) != 1 || open[0].State != "方案被否" {
			t.Errorf("%s: the ledger changed: %+v", tc.name, open)
		}
	}

	h, s := frontsHandler(t)
	w := httptest.NewRecorder()
	h.HandleFrontEdit(w, frontReq("POST", `{"id":"jd-tgw","state":"路线图已改","waiting":""}`, local, nil))
	var got front.Front
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	if got.State != "路线图已改" || got.Waiting != "" || got.Name != "JD TGW 迁移" {
		t.Errorf("edited = %+v", got)
	}
	w = httptest.NewRecorder()
	h.HandleFrontEdit(w, frontReq("POST", `{"id":"jd-tgw","close":true}`, local, nil))
	if w.Code != http.StatusOK || len(s.Open()) != 0 {
		t.Errorf("close: %d, open %+v", w.Code, s.Open())
	}
}

// The ledger stays on the machine that keeps it: a reverse node's `api`
// calls reach neither route.
func TestFrontsNotANodeRoute(t *testing.T) {
	for _, r := range []string{"GET /api/planner/fronts", "POST /api/planner/fronts"} {
		if node.IsAPIRoute(r) {
			t.Errorf("%s is forwarded to nodes", r)
		}
	}
}

// The open items list with the line each shows under, to any caller past
// the page's auth (§15.23, D143).
func TestHandleFrontItems(t *testing.T) {
	h, s := frontsHandler(t)
	w := httptest.NewRecorder()
	h.HandleFronts(w, frontReq("GET", "", "127.0.0.1:50000", nil))
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("with no item: %s", w.Body)
	}
	if _, _, err := s.AddItem(front.Item{Text: "迁完剩下的调用点", Workspace: "ws_ab12cd34", Front: "jd-tgw", Source: "task:9f3c2a71d0", From: front.FromReport}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddItem(front.Item{Text: "没归线的", Workspace: "ws_ab12cd34", Source: "https://github.com/owner/repo/pull/12", From: front.FromPlanner}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.HandleFronts(w, frontReq("GET", "", "192.168.1.20:50000", nil))
	var got frontsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(got.Items) != 2 || got.Items[0].Front != "jd-tgw" || got.Items[0].From != front.FromReport || got.Items[0].Source != "task:9f3c2a71d0" ||
		got.Items[1].Front != "" || got.Items[1].Workspace != "ws_ab12cd34" || got.Writable {
		t.Errorf("items = %+v, writable %v", got.Items, got.Writable)
	}
}

// The user adds an open item and strikes one only straight from loopback,
// one thing a request; an added one is the user's, a struck one is closed
// by the user and off the list.
func TestHandleFrontItemEdit(t *testing.T) {
	const local = "127.0.0.1:50000"
	const add = `{"item":{"text":"给 README 补上安装步骤","workspace":"ws_ab12cd34","front":"jd-tgw"}}`
	seed := func(s *front.Store) string {
		t.Helper()
		it, _, err := s.AddItem(front.Item{Text: "迁完剩下的调用点", Source: "task:9f3c2a71d0", From: front.FromReport})
		if err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	for _, tc := range []struct {
		name, body, remote string
		hdr                map[string]string
		code               int
	}{
		{"add from another machine", add, "10.0.0.5:50000", nil, http.StatusForbidden},
		{"add forwarded", add, local, map[string]string{"X-Forwarded-For": "127.0.0.1"}, http.StatusForbidden},
		{"add by a reverse node's api call", add, "", nil, http.StatusForbidden},
		{"strike from another machine", `{"item_done":"ID"}`, "10.0.0.5:50000", nil, http.StatusForbidden},
		{"strike by a reverse node's api call", `{"item_done":"ID"}`, "", nil, http.StatusForbidden},
		{"add and strike", `{"item":{"text":"x"},"item_done":"ID"}`, local, nil, http.StatusBadRequest},
		{"add and edit a line", `{"id":"jd-tgw","state":"s","item":{"text":"x"}}`, local, nil, http.StatusBadRequest},
		{"strike and close a line", `{"id":"jd-tgw","close":true,"item_done":"ID"}`, local, nil, http.StatusBadRequest},
		{"no text", `{"item":{"text":" "}}`, local, nil, http.StatusBadRequest},
		{"long text", `{"item":{"text":"` + strings.Repeat("字", front.ItemRunes+1) + `"}}`, local, nil, http.StatusBadRequest},
		{"a workspace's name", `{"item":{"text":"x","workspace":"repo"}}`, local, nil, http.StatusBadRequest},
		{"unknown line", `{"item":{"text":"x","front":"nope"}}`, local, nil, http.StatusNotFound},
		{"unknown item field", `{"item":{"text":"x","source":"task:1"}}`, local, nil, http.StatusBadRequest},
		{"unknown item", `{"item_done":"i0000000"}`, local, nil, http.StatusNotFound},
	} {
		h, s := frontsHandler(t)
		id := seed(s)
		w := httptest.NewRecorder()
		h.HandleFrontEdit(w, frontReq("POST", strings.ReplaceAll(tc.body, "ID", id), tc.remote, tc.hdr))
		if w.Code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, w.Code, w.Body, tc.code)
		}
		if items := s.Items(); len(items) != 1 || items[0].ID != id {
			t.Errorf("%s: the items changed: %+v", tc.name, items)
		}
		if open := s.Open(); len(open) != 1 || open[0].State != "方案被否" {
			t.Errorf("%s: the ledger changed: %+v", tc.name, open)
		}
	}

	h, s := frontsHandler(t)
	id := seed(s)
	w := httptest.NewRecorder()
	h.HandleFrontEdit(w, frontReq("POST", add, local, nil))
	var got front.Item
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if !front.ItemID(got.ID) || got.Text != "给 README 补上安装步骤" || got.From != front.FromUser || got.Front != "jd-tgw" || got.Workspace != "ws_ab12cd34" {
		t.Errorf("added = %+v", got)
	}
	// One under no line and no workspace.
	w = httptest.NewRecorder()
	h.HandleFrontEdit(w, frontReq("POST", `{"item":{"text":"没归线的"}}`, local, nil))
	if w.Code != http.StatusOK || len(s.Items()) != 3 {
		t.Fatalf("add a loose one: %d %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	h.HandleFrontEdit(w, frontReq("POST", `{"item_done":"`+id+`"}`, local, nil))
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("strike: %d %s", w.Code, w.Body)
	}
	if got.ID != id || got.ClosedBy != front.ByUser || got.Closed.IsZero() {
		t.Errorf("struck = %+v", got)
	}
	if items := s.Items(); len(items) != 2 || items[0].ID == id || items[1].ID == id {
		t.Errorf("after the strike: %+v", items)
	}
	// Struck twice (two tabs) is struck.
	w = httptest.NewRecorder()
	h.HandleFrontEdit(w, frontReq("POST", `{"item_done":"`+id+`"}`, local, nil))
	if w.Code != http.StatusOK {
		t.Errorf("strike again: %d %s", w.Code, w.Body)
	}
}
