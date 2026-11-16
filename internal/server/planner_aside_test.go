package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dashplanner "github.com/naozhi/naozhi/internal/dashboard/ext/planner"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/journal"
)

// TestPlannerAside: opening an aside chat (单独聊, §15.14 ⑥) leaves one line
// in the Planner conversation naming the session, journaled like the
// gateway's other notes and never a turn of the Planner's; only a dashboard
// session with a title can be one, the Planner off refuses, and a node's
// Planner page reaches its own.
func TestPlannerAside(t *testing.T) {
	const key = "dashboard:direct:2026-11-16-101500-1-naozhi:general"
	for _, tc := range []struct {
		name   string
		body   string
		off    bool
		status int
		noted  string
	}{
		{name: "noted", body: `{"key":"` + key + `","title":"  BTC 现在\n多少钱？ "}`, status: http.StatusOK,
			noted: "💬 " + key + " 你开了单独聊：BTC 现在 多少钱？"},
		{name: "a long title is cut", body: `{"key":"` + key + `","title":"` + strings.Repeat("长", 80) + `"}`, status: http.StatusOK,
			noted: "💬 " + key + " 你开了单独聊：" + strings.Repeat("长", 60) + "…"},
		{name: "no title", body: `{"key":"` + key + `","title":" "}`, status: http.StatusBadRequest},
		{name: "the Planner's own", body: `{"key":"planner:main","title":"x"}`, status: http.StatusBadRequest},
		{name: "a dispatched session", body: `{"key":"task:abc","title":"x"}`, status: http.StatusBadRequest},
		{name: "an IM session", body: `{"key":"feishu:direct:alice:general","title":"x"}`, status: http.StatusBadRequest},
		{name: "Planner off", body: `{"key":"` + key + `","title":"x"}`, off: true, status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := agent.New(agent.Config{Enabled: !tc.off, Layout: datadir.FromRoot(t.TempDir()), Home: t.TempDir(), Sessions: plannerFileSessions{}, Journal: j})
			turns := &plannerFileTurns{}
			h := dashplanner.New(a, nil, nil, nil)
			r := httptest.NewRequest(http.MethodPost, "/api/planner/aside", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.HandleAside(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			notes, err := a.Notes()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, n := range notes {
				got = append(got, n.Text)
			}
			if tc.noted == "" && len(got) != 0 || tc.noted != "" && (len(got) != 1 || got[0] != tc.noted) {
				t.Errorf("notes = %q, want %q", got, tc.noted)
			}
			if texts, _ := turns.sent(); len(texts) != 0 {
				t.Errorf("the aside reached a Planner turn: %q", texts)
			}
			if tc.status == http.StatusOK {
				var body map[string]string
				if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["status"] != "noted" {
					t.Errorf("body %s", w.Body.String())
				}
			}
		})
	}
	if !node.IsAPIRoute("POST /api/planner/aside") {
		t.Error("a node's Planner page cannot note an aside on its node")
	}
}
