package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// These tests exercise the four response shapes RFC v4 §3.5.1 specifies:
//
//   - 200 [EventEntry...]            happy path (linker resolved + jsonl readable)
//   - 202 {"status":"pending"}       linker has not seen the task_id yet
//   - 404                            tombstone (Resolved but empty), missing file, bad key
//   - 400                            invalid key / task_id / path / after / limit
//
// The handler is exercised directly via httptest.NewRecorder, bypassing auth
// middleware (tested separately). A stub linkerFor lookup wires each test to
// a hand-rolled SubagentLinker — we don't need a live *cli.Process for the
// HTTP-level contract.

const testAgentEventsKey = "dashboard:direct:test-agent-events:general"

func agentEventsReq(key, taskID, after, limit string) *http.Request {
	q := url.Values{}
	q.Set("key", key)
	q.Set("task_id", taskID)
	if after != "" {
		q.Set("after", after)
	}
	if limit != "" {
		q.Set("limit", limit)
	}
	return httptest.NewRequest(http.MethodGet, "/api/sessions/agent_events?"+q.Encode(), nil)
}

func writeTranscript(t *testing.T, dir, hex string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, "agent-"+hex+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

// claudeProjectsTestRoot creates a fake ~/.claude/projects tree rooted at a
// TempDir and returns (subagentsDir, cleanup). Point HOME at the temp root
// for the test's duration so cli.SeedFromHistory's prefix check accepts the
// path; real production code only ever reads files under ~/.claude/projects.
func claudeProjectsTestRoot(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	sub := filepath.Join(home, ".claude", "projects", "-test", "sess", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return sub
}

func TestAgentEvents_Happy(t *testing.T) {
	// Cannot run in parallel — t.Setenv("HOME") mutates process state.
	dir := claudeProjectsTestRoot(t)
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := writeTranscript(t, dir, "aaaaaaaaaaaaaaaaa", []string{line})

	linker := cli.NewSubagentLinker()
	linker.SeedFromHistory([]cli.EventEntry{{
		Type:            "task_start",
		ToolUseID:       "toolu_T",
		TaskID:          "t1",
		InternalAgentID: "agent-aaaaaaaaaaaaaaaaa",
		JSONLPath:       path,
		Subagent:        "worker",
	}})

	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker {
			if k != testAgentEventsKey {
				return nil
			}
			return linker
		},
	}

	w := httptest.NewRecorder()
	h.handleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t1", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got []cli.EventEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Type != "text" || got[0].Summary != "hi" {
		t.Errorf("got=%+v", got)
	}
}

func TestAgentEvents_Pending_WhenLinkerUnaware(t *testing.T) {
	t.Parallel()
	linker := cli.NewSubagentLinker()
	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return linker },
	}
	w := httptest.NewRecorder()
	h.handleAgentEvents(w, agentEventsReq(testAgentEventsKey, "tunknown", "", ""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "pending" {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestAgentEvents_Tombstone_404(t *testing.T) {
	t.Parallel()
	linker := cli.NewSubagentLinker()
	// Seed a tombstone-ish record — Resolved but empty InternalAgentID is how
	// a real tombstone from Resolve step 4 timeout looks. SeedFromHistory
	// requires InternalAgentID+JSONLPath non-empty, so simulate by letting
	// Resolve tombstone a missing task via a bad projectDir.
	linker.SetContext(t.TempDir(), "bogus-session-uuid")
	// Force a zero-retry Resolve so the test finishes fast.
	linker.ConfigureForTest(1*1e6, 0, 1*1e6) // 1ms retryInterval, 0 retries, 1ms cacheTTL
	info, _ := linker.Resolve("tmissing", "toolu_X", "ghost", "", 0)
	if info.Resolved != true || info.InternalAgentID != "" {
		t.Fatalf("expected tombstone, got %+v", info)
	}

	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return linker },
	}
	w := httptest.NewRecorder()
	h.handleAgentEvents(w, agentEventsReq(testAgentEventsKey, "tmissing", "", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestAgentEvents_InvalidKey_400(t *testing.T) {
	t.Parallel()
	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return cli.NewSubagentLinker() },
	}
	w := httptest.NewRecorder()
	h.handleAgentEvents(w, agentEventsReq("", "t1", "", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestAgentEvents_InvalidTaskID_400(t *testing.T) {
	t.Parallel()
	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return cli.NewSubagentLinker() },
	}
	w := httptest.NewRecorder()
	h.handleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t/../escape", "", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestAgentEvents_AfterFilter(t *testing.T) {
	// Cannot run in parallel — t.Setenv("HOME") mutates process state.
	dir := claudeProjectsTestRoot(t)
	line1 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	line2 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"new"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:05Z"}`
	path := writeTranscript(t, dir, "bbbbbbbbbbbbbbbbb", []string{line1, line2})

	linker := cli.NewSubagentLinker()
	linker.SeedFromHistory([]cli.EventEntry{{
		Type:            "task_start",
		ToolUseID:       "toolu_A",
		TaskID:          "ta",
		InternalAgentID: "agent-bbbbbbbbbbbbbbbbb",
		JSONLPath:       path,
	}})

	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return linker },
	}
	// after=2026-05-10T10:00:03Z → only line2 should remain. Parse as ms.
	// 2026-05-10T10:00:03Z = 1778407203000
	w := httptest.NewRecorder()
	h.handleAgentEvents(w, agentEventsReq(testAgentEventsKey, "ta", "1778407203000", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got []cli.EventEntry
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Summary != "new" {
		t.Errorf("after filter: got=%+v", got)
	}
}

// ─── tool_result endpoint ───────────────────────────────────────────────

func TestToolResult_Happy(t *testing.T) {
	t.Parallel()
	projectSession := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectSession, "tool-results"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payloadPath := filepath.Join(projectSession, "tool-results", "abc12.txt")
	if err := os.WriteFile(payloadPath, []byte("large output body"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	linker := cli.NewSubagentLinker()
	// Split the tempdir into (projectDir, sessionID) so ProjectSessionDir returns projectSession.
	linker.SetContext(filepath.Dir(projectSession), filepath.Base(projectSession))

	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return linker },
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/tool_result?key="+testAgentEventsKey+"&path=tool-results/abc12.txt", nil)
	w := httptest.NewRecorder()
	h.handleToolResult(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body, _ := io.ReadAll(w.Body)
	if string(body) != "large output body" {
		t.Errorf("body=%q", body)
	}
}

func TestToolResult_PathTraversal_400(t *testing.T) {
	t.Parallel()
	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return cli.NewSubagentLinker() },
	}
	for _, p := range []string{
		"../../etc/passwd",
		"tool-results/../../../etc/passwd",
		"tool-results/abc",     // no extension
		"/etc/passwd",          // absolute
		"tool-results/a.exe",   // bad extension
		"tool-results/a b.txt", // space
	} {
		q := url.Values{}
		q.Set("key", testAgentEventsKey)
		q.Set("path", p)
		req := httptest.NewRequest(http.MethodGet,
			"/api/sessions/tool_result?"+q.Encode(), nil)
		w := httptest.NewRecorder()
		h.handleToolResult(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("path=%q accepted (code=%d)", p, w.Code)
		}
	}
}

func TestToolResult_NoLinker_404(t *testing.T) {
	t.Parallel()
	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return nil },
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/tool_result?key="+testAgentEventsKey+"&path=tool-results/abc12.txt", nil)
	w := httptest.NewRecorder()
	h.handleToolResult(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", w.Code)
	}
}

func TestToolResult_Oversize_413(t *testing.T) {
	t.Parallel()
	projectSession := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectSession, "tool-results"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bigPath := filepath.Join(projectSession, "tool-results", "big.txt")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Past the 16 MB cap.
	if err := f.Truncate(toolResultMaxBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	linker := cli.NewSubagentLinker()
	linker.SetContext(filepath.Dir(projectSession), filepath.Base(projectSession))
	h := &AgentEventsHandlers{
		linkerFor: func(k string) *cli.SubagentLinker { return linker },
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/tool_result?key="+testAgentEventsKey+"&path=tool-results/big.txt", nil)
	w := httptest.NewRecorder()
	h.handleToolResult(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d want 413", w.Code)
	}
}
