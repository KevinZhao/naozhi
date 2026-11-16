package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	dashplanner "github.com/naozhi/naozhi/internal/dashboard/ext/planner"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/session"
)

// asideTurns is the turn layer as the Planner sees it, keeping the lines the
// gateway shows in its live conversation.
type asideTurns struct {
	plannerFileTurns
	mu    sync.Mutex
	lines []string
}

func (f *asideTurns) PlannerNote(_, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, text)
}

func (f *asideTurns) noted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

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
			lay := datadir.FromRoot(t.TempDir())
			a := agent.New(agent.Config{Enabled: !tc.off, Layout: lay, Home: t.TempDir(), Sessions: plannerFileSessions{}, Journal: j})
			turns := &asideTurns{}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			a.Start(ctx, turns, nil)
			if !tc.off {
				// As in TestPlannerSendFiles: the loop's first write must
				// end before TempDir's cleanup.
				waitFile(t, lay.PlannerAgentPath())
			}
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
			if live := turns.noted(); tc.noted == "" && len(live) != 0 || tc.noted != "" && (len(live) != 1 || live[0] != tc.noted) {
				t.Errorf("lines in the live conversation = %q, want %q", live, tc.noted)
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

// TestPlannerAsideEnd: 结束 on an aside chat (§15.14 ⑥) puts it away to the
// history (Router.Archive, which keeps its event log for a resume under the
// key), not removed as the sidebar's 移除 does; only a dashboard session, and
// a node's Planner page reaches its own.
func TestPlannerAsideEnd(t *testing.T) {
	const key = "dashboard:direct:aside-0123456789abcdef:general"
	events := filepath.Join(t.TempDir(), "events")
	router := session.NewRouter(session.RouterConfig{Workspace: t.TempDir(), EventLogDir: events})
	t.Cleanup(router.Shutdown)
	proc := session.NewTestProcess()
	router.InjectSession(key, proc).SetWorkspaceForTest("/w")
	// Its event log, as its turns wrote it.
	if err := os.WriteFile(persist.LogPath(events, key), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	_, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	end := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/planner/aside/end", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		hs.plannerH.HandleAsideEnd(w, r)
		return w
	}
	if w := end(`{"key":"feishu:direct:alice:general"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an IM session: status %d, want 400", w.Code)
	}
	if router.SessionFor("feishu:direct:alice:general") == nil {
		t.Error("an IM session was put away")
	}
	if w := end(`{"key":"` + key + `"}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if router.SessionFor(key) != nil || proc.Alive() {
		t.Error("the aside is still in the session list, or its process runs")
	}
	if _, err := os.Stat(persist.LogPath(events, key)); err != nil {
		t.Errorf("its event log went with it: %v", err)
	}
	if w := end(`{"key":"` + key + `"}`); w.Code != http.StatusNotFound {
		t.Errorf("ended twice: status %d, want 404", w.Code)
	}
	if !node.IsAPIRoute("POST /api/planner/aside/end") {
		t.Error("a node's Planner page cannot end an aside on its node")
	}
}
