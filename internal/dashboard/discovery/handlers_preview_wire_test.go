package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
)

const (
	previewWireSessionID = "00000000-0000-0000-0000-0000000013b2"
	previewWireSecret    = "sk-ant-api03-QQQQQQQQQQQQQQQQQQQQQQQQ"
)

type previewConn struct {
	node.Conn
	entries []clievent.EventEntry
}

func (c previewConn) FetchDiscoveredPreview(context.Context, string) ([]clievent.EventEntry, error) {
	return c.entries, nil
}

type previewNodeAccess struct{ conn node.Conn }

func (previewNodeAccess) HasNodes() bool { return true }
func (a previewNodeAccess) LookupNode(http.ResponseWriter, string) (node.Conn, bool) {
	return a.conn, true
}

func servePreview(t *testing.T, h *Handlers, q url.Values) string {
	t.Helper()
	q.Set("session_id", previewWireSessionID)
	rec := httptest.NewRecorder()
	h.HandlePreview(rec, httptest.NewRequest(http.MethodGet, "/api/discovered/preview?"+q.Encode(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func assertWireView(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("preview lost the entry (want %q): %s", want, body)
	}
	for _, leak := range []string{"jsonl_path", "internal_agent_id", previewWireSecret} {
		if strings.Contains(body, leak) {
			t.Errorf("preview body carries %q: %s", leak, body)
		}
	}
}

// TestHandlePreview_Remote_SendsTheWireView: a peer's preview is re-served to
// the browser as the wire view, whatever the peer sent.
func TestHandlePreview_Remote_SendsTheWireView(t *testing.T) {
	t.Parallel()
	conn := previewConn{entries: []clievent.EventEntry{{
		Time: 1, Type: "task_start", Summary: "token " + previewWireSecret,
		InternalAgentID: "agent-abc", JSONLPath: "/home/u/.claude/projects/p/s/subagents/agent-abc.jsonl",
	}}}
	h := New(Deps{NodeAccess: previewNodeAccess{conn: conn}})
	assertWireView(t, servePreview(t, h, url.Values{"node": {"peer"}}), `"task_start"`)
}

// TestHandlePreview_Local_SendsTheWireView: a credential in a discovered
// session's transcript is redacted in the preview.
func TestHandlePreview_Local_SendsTheWireView(t *testing.T) {
	t.Parallel()
	claudeDir := t.TempDir()
	cwd := "/home/u/preview-wire"
	path := claudefs.SessionJSONL(claudeDir, cwd, previewWireSessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"my key is ` + previewWireSecret + `"}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{ClaudeDir: claudeDir})
	assertWireView(t, servePreview(t, h, url.Values{"cwd": {cwd}}), "my key is")
}
