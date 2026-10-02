package session

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

const wireSecret = "sk-ant-api03-CCCCCCCCCCCCCCCCCCCCCCCC"

func wireFixture() []clievent.EventEntry {
	return []clievent.EventEntry{
		{Time: 1000, UUID: "a", Type: "task_start", TaskID: "t1", JSONLPath: "/home/u/.claude/projects/p/s/subagents/agent-x.jsonl", InternalAgentID: "agent-x"},
		{Time: 2000, UUID: "b", Type: "text", Summary: "key " + wireSecret},
	}
}

func assertWireBody(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{wireSecret, "jsonl_path", "internal_agent_id", "/home/u/"} {
		if strings.Contains(body, leak) {
			t.Errorf("events response carries %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, `"task_id":"t1"`) {
		t.Errorf("events response lost the wire fields: %s", body)
	}
}

// Every REST events path leaves through clievent.ForWire: no local
// agent-linkage field, no credential shape.
func TestHandleEvents_WireView(t *testing.T) {
	const key = "feishu:p2p:alice:general"
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(t.TempDir(), "sessions.json")})
	t.Cleanup(r.Shutdown)
	proc := sessionpkg.NewTestProcess()
	for _, e := range wireFixture() {
		proc.EventLog.Append(e)
	}
	r.InjectSession(key, proc)
	h := New(Deps{Router: realRouter{r}})
	for _, q := range []string{"", "&after=1", "&before=3000&limit=10"} {
		rec := httptest.NewRecorder()
		h.HandleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+key+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", q, rec.Code, rec.Body.String())
		}
		assertWireBody(t, rec.Body.String())
	}

	remote := New(Deps{NodeAccess: fakeEventsNodeAccessor{conn: &fakeEventsConn{entries: wireFixture()}}})
	for _, q := range []string{"", "&before=3000&limit=10"} {
		rec := httptest.NewRecorder()
		remote.HandleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=feishu:p2p:u1:general&node=peer"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("remote %q: status=%d body=%s", q, rec.Code, rec.Body.String())
		}
		assertWireBody(t, rec.Body.String())
	}
}
