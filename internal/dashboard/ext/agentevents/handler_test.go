package agentevents

import (
	"context"
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
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
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

	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker {
			if k != testAgentEventsKey {
				return nil
			}
			return linker
		},
	}

	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t1", "", ""))
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
	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return linker },
	}
	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "tunknown", "", ""))
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
	info, _ := linker.Resolve(context.Background(), "tmissing", "toolu_X", "ghost", "", 0)
	if info.Resolved != true || info.InternalAgentID != "" {
		t.Fatalf("expected tombstone, got %+v", info)
	}

	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return linker },
	}
	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "tmissing", "", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestAgentEvents_InvalidKey_400(t *testing.T) {
	t.Parallel()
	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return cli.NewSubagentLinker() },
	}
	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq("", "t1", "", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

func TestAgentEvents_InvalidTaskID_400(t *testing.T) {
	t.Parallel()
	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return cli.NewSubagentLinker() },
	}
	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "t/../escape", "", ""))
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

	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return linker },
	}
	// after=2026-05-10T10:00:03Z → only line2 should remain. Parse as ms.
	// 2026-05-10T10:00:03Z = 1778407203000
	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "ta", "1778407203000", ""))
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

	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return linker },
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/tool_result?key="+testAgentEventsKey+"&path=tool-results/abc12.txt", nil)
	w := httptest.NewRecorder()
	h.HandleToolResult(w, req)
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
	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return cli.NewSubagentLinker() },
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
		h.HandleToolResult(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("path=%q accepted (code=%d)", p, w.Code)
		}
	}
}

func TestToolResult_NoLinker_404(t *testing.T) {
	t.Parallel()
	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return nil },
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/tool_result?key="+testAgentEventsKey+"&path=tool-results/abc12.txt", nil)
	w := httptest.NewRecorder()
	h.HandleToolResult(w, req)
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
	h := &Handler{
		linkerFor: func(k string) agentlink.AgentLinker { return linker },
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/tool_result?key="+testAgentEventsKey+"&path=tool-results/big.txt", nil)
	w := httptest.NewRecorder()
	h.HandleToolResult(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d want 413", w.Code)
	}
}

// ---------------------------------------------------------------------------
// R248-TEST-5 — AgentLinker typed-nil guard
// ---------------------------------------------------------------------------

// TestLinkerForSession_TypedNilGuard pins the typed-nil interface guard at
// dashboard_agent_events.go:linkerForSession's `if concrete == nil` line.
// ManagedSession.SubagentLinker() returns *cli.SubagentLinker — a concrete
// pointer type — and a nil concrete return becomes a NON-nil agentlink.AgentLinker
// interface value when promoted directly (Go's classic typed-nil hazard).
// Without the explicit guard, callers checking `if linker == nil` would see
// false and dereference the nil pointer on the next method call.
//
// To exercise the production code path (no linkerFor injection), this test
// stands up a real *session.Router, injects a session backed by a non-cli.Process
// (session.TestProcess satisfies processIface but is not *cli.Process), and
// observes that:
//
//   - linkerForSession returns an interface value that compares == nil
//     (the guard converted typed-nil → untyped nil).
//   - The HTTP handler routed through this lookup returns 404 "unknown task",
//     proving the guard prevents a nil-dereference panic from leaking.
//
// R248-TEST-5.
func TestLinkerForSession_TypedNilGuard(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	// TestProcess satisfies processIface but is NOT a *cli.Process, so
	// ManagedSession.loadCliProcess returns untyped nil and SubagentLinker
	// returns a typed-nil *cli.SubagentLinker. That is exactly the input
	// the linkerForSession guard is designed for.
	router.InjectSession(testAgentEventsKey, session.NewTestProcess())

	h := &Handler{router: router}

	// Direct check: linkerForSession converts typed-nil → untyped nil so
	// callers can use the idiomatic `if linker == nil` form.
	got := h.linkerForSession(testAgentEventsKey)
	if got != nil {
		t.Errorf("linkerForSession returned non-nil interface (%T %v) for a "+
			"non-cli.Process-backed session; the typed-nil guard at "+
			"dashboard_agent_events.go must convert to untyped nil so callers' "+
			"`if linker == nil` works idiomatically. R248-TEST-5.", got, got)
	}

	// End-to-end: the HTTP handler must return 404 (no live linker) rather
	// than panic. taskID uses an alphanumeric form so the early task_id
	// validator does not short-circuit before reaching linkerForSession.
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/agent_events?key="+testAgentEventsKey+"&task_id=tabc", nil)
	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("HTTP status = %d, want 404; the typed-nil-promoted linker "+
			"would slip past the nil guard and either reach linker.QueryOrResolveFast "+
			"(panic) or write a misleading 200/202. R248-TEST-5.", w.Code)
	}
}

// stubLinker implements agentlink.AgentLinker for tests that need a specific
// LinkInfo returned from QueryOrResolveFast without going through
// SeedFromHistory (which has its own path-validation gate).
type stubLinker struct {
	info     cli.LinkInfo
	resolved bool
}

func (s *stubLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {}
func (s *stubLinker) Query(taskID string) (cli.LinkInfo, bool) {
	return s.info, s.resolved
}
func (s *stubLinker) QueryOrResolveFast(taskID string) (cli.LinkInfo, bool) {
	return s.info, s.resolved
}
func (s *stubLinker) ProjectSessionDir() string { return "" }

// TestAgentEvents_JSONLPathOutsideAllowedRoot pins R112714-SEC-3: when
// info.JSONLPath does not live under h.allowedRoot the handler must return
// 404 rather than opening an unchecked path. We use a stub linker that
// returns an outside-root JSONLPath directly, bypassing SeedFromHistory's
// own path gate (which is tested separately in the cli package).
func TestAgentEvents_JSONLPathOutsideAllowedRoot(t *testing.T) {
	t.Parallel()
	allowedRoot := t.TempDir() // acts as the "allowed" root

	// Write a real file OUTSIDE allowedRoot.
	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret.jsonl")
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"secret"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	if err := os.WriteFile(secretPath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Stub linker returns the outside-root path directly.
	linker := &stubLinker{
		info: cli.LinkInfo{
			InternalAgentID: "agent-sec3aaaaaaaaaaaa",
			JSONLPath:       secretPath,
			Resolved:        true,
		},
		resolved: true,
	}

	h := &Handler{
		allowedRoot: allowedRoot,
		linkerFor:   func(k string) agentlink.AgentLinker { return linker },
	}

	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "tsec3", "", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 — JSONLPath outside allowedRoot must be rejected [R112714-SEC-3]", w.Code)
	}
}

// TestAgentEvents_JSONLPathUnderAllowedRoot_Accepted pins R112714-SEC-3:
// a legitimate JSONLPath under allowedRoot must NOT be rejected by the
// path check (regression guard against over-rejection).
func TestAgentEvents_JSONLPathUnderAllowedRoot_Accepted(t *testing.T) {
	t.Parallel()
	// EvalSymlinks to match production: New() stores the resolved root, and
	// jsonlPathUnderAllowedRoot resolves the request path before comparing.
	// On macOS t.TempDir() returns an unresolved /var/... path that the
	// in-handler EvalSymlinks expands to /private/var/..., so the raw
	// TempDir would spuriously fail the prefix check.
	allowedRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir): %v", err)
	}

	// Write a transcript file INSIDE allowedRoot.
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	jsonlPath := filepath.Join(allowedRoot, "agent-ccc.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	linker := &stubLinker{
		info: cli.LinkInfo{
			InternalAgentID: "agent-ccccccccccccccccc",
			JSONLPath:       jsonlPath,
			Resolved:        true,
		},
		resolved: true,
	}

	h := &Handler{
		allowedRoot: allowedRoot,
		linkerFor:   func(k string) agentlink.AgentLinker { return linker },
	}

	w := httptest.NewRecorder()
	h.HandleAgentEvents(w, agentEventsReq(testAgentEventsKey, "tok1", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s — legitimate JSONLPath under allowedRoot must be accepted [R112714-SEC-3]",
			w.Code, w.Body.String())
	}
}
