package agentevents

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// TestAgentEvents_AfterReadmitsWatermarkMillisecond (#2432 item 5): the HTTP
// poll fallback sends after=<time of the last rendered entry>. Real subagent
// transcripts carry same-timestamp siblings (multi-block assistant lines and
// consecutive thinking/text lines share one ms), so when a page's `limit`
// cuts between them the strict `Time > after` cursor dropped the second
// forever. The read must be `Time >= after`; agent_view.js dedups the
// replayed first entry by content key.
func TestAgentEvents_AfterReadmitsWatermarkMillisecond(t *testing.T) {
	// Cannot run in parallel — t.Setenv("HOME") mutates process state.
	dir := claudeProjectsTestRoot(t)
	// Shape observed in ~/.claude/projects/*/subagents/agent-*.jsonl:
	// a thinking line and a text line with the identical timestamp.
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]},"sessionId":"s","timestamp":"2026-09-02T04:38:14.871Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]},"sessionId":"s","timestamp":"2026-09-02T04:38:14.871Z"}`,
	}
	path := writeTranscript(t, dir, "bbbbbbbbbbbbbbbbb", lines)

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

	fetch := func(after, limit string) []cli.EventEntry {
		t.Helper()
		w := httptest.NewRecorder()
		h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t1", after, limit))
		if w.Code != http.StatusOK {
			t.Fatalf("after=%s limit=%s: status=%d body=%s", after, limit, w.Code, w.Body.String())
		}
		var got []cli.EventEntry
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	// Page 1: limit cuts right after the thinking entry.
	first := fetch("", "1")
	if len(first) != 1 || first[0].Type != "thinking" || first[0].Time == 0 {
		t.Fatalf("page1 = %+v, want the single thinking entry with a real time", first)
	}
	// Page 2: the client polls with after=<page1 watermark>.
	second := fetch(strconv.FormatInt(first[0].Time, 10), "")
	types := make([]string, 0, len(second))
	for _, e := range second {
		types = append(types, e.Type)
	}
	if len(second) != 2 || second[0].Type != "thinking" || second[1].Type != "text" {
		t.Fatalf("#2432 item 5 regression: after=%d returned %v, want [thinking text] (same-ms text sibling must be replayed)", first[0].Time, types)
	}
}
