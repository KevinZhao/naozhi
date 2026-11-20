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
	if got := strings.TrimSpace(w.Body.String()); got != `{"fronts":[],"writable":false}` {
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
