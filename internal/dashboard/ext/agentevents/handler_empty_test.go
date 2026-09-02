package agentevents

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// TestAgentEvents_EmptyTranscriptReturnsEmptyArray: a resolved task whose
// jsonl has no entries yet must serialise as `[]`, not `null`. agent_view.js
// treats a falsy body as "no data" and never subscribes to the live WS feed,
// leaving a permanent spinner.
func TestAgentEvents_EmptyTranscriptReturnsEmptyArray(t *testing.T) {
	dir := claudeProjectsTestRoot(t)
	path := writeTranscript(t, dir, "bbbbbbbbbbbbbbbbb", nil)

	linker := cli.NewSubagentLinker()
	linker.SeedFromHistory([]cli.EventEntry{{
		Type:            "task_start",
		ToolUseID:       "toolu_T",
		TaskID:          "t1",
		InternalAgentID: "agent-bbbbbbbbbbbbbbbbb",
		JSONLPath:       path,
		Subagent:        "worker",
	}})
	h := &Handler{linkerFor: func(string) agentlink.AgentLinker { return linker }}

	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t1", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
}
