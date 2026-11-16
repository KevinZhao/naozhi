package scratch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

type noBroadcast struct{}

func (noBroadcast) BroadcastSessionsUpdate() {}

// An aside on a Planner reply (§15.14 ⑥) is an ordinary chat: the default
// agent's opts on the default workspace, account and backend, not the
// Planner's, and only the quote goes in, not the turns around it, which
// carry the gateway's snapshot. Promoted it becomes a dashboard session on
// the default agent (a 单独聊), where a 2-segment source key used to make
// the promote fail.
func TestPlannerSourceAsideIsOrdinaryChat(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	pl := r.InjectSession(sessionkey.PlannerMainKey, nil)
	pl.SetAccessProfile("planner-profile")
	pl.SetModel("claude-opus-5-5[1m]")
	pl.SetWorkspaceForTest("/state/planner/work")
	now := time.Now().UnixMilli()
	pl.InjectHistory([]clievent.EventEntry{
		{Type: "user", Detail: "<naozhi-snapshot at=\"x\">会话 secret</naozhi-snapshot>\n\n现在都在干什么？", Time: now - 2000},
		{Type: "text", Detail: "修复批次在跑。", Time: now - 1000},
	})
	pool := session.NewScratchPool(r, 4, time.Minute)
	h := New(Deps{
		Broadcaster: noBroadcast{},
		Router:      sourceRouter{r},
		Pool:        pool,
		Agents:      map[string]session.AgentOpts{"general": {Model: "registry-default", AccessProfile: "company"}},
	})

	body := strings.NewReader(`{"source_key":"planner:main","quote":"修复批次在跑。","source_message_time":` + jsonInt(now-1000) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", body)
	rec := httptest.NewRecorder()
	h.HandleOpen(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	sc := pool.Get(resp.ScratchID)
	if sc == nil {
		t.Fatalf("scratch %q not in pool", resp.ScratchID)
	}
	if sc.BaseOpts.Model != "registry-default" || sc.BaseOpts.AccessProfile != "" || sc.BaseOpts.Workspace != "" || resp.Workspace != "" || sc.AgentID != "general" {
		t.Errorf("aside opts = model %q profile %q workspace %q (resp %q) agent %q, want the default agent's on the default workspace and account",
			sc.BaseOpts.Model, sc.BaseOpts.AccessProfile, sc.BaseOpts.Workspace, resp.Workspace, sc.AgentID)
	}
	if resp.ContextTurns != 0 || strings.Contains(sc.BaseOpts.SystemPrompt, "secret") {
		t.Errorf("context turns = %d, prompt %q: the Planner's turns went in", resp.ContextTurns, sc.BaseOpts.SystemPrompt)
	}

	// The scratch's process, as the first send in the drawer makes it.
	r.InjectSession(sc.Key, nil)
	preq := httptest.NewRequest(http.MethodPost, "/api/scratch/"+sc.ID+"/promote", nil)
	preq.SetPathValue("id", sc.ID)
	prec := httptest.NewRecorder()
	h.HandlePromote(prec, preq)
	if prec.Code != http.StatusOK {
		t.Fatalf("promote status=%d body=%s", prec.Code, prec.Body.String())
	}
	var pr promoteResponse
	if err := json.Unmarshal(prec.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^dashboard:direct:aside-[0-9a-f]{16}:general$`).MatchString(pr.Key) {
		t.Errorf("promoted key = %q, want a dashboard session on the default agent", pr.Key)
	}
	if r.SessionFor(pr.Key) == nil || r.SessionFor(sc.Key) != nil {
		t.Errorf("the scratch's process did not move to %q", pr.Key)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
