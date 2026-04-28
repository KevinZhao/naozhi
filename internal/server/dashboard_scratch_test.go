package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// newScratchTestServer wires a newTestServer with a live ScratchPool + handler,
// matching the production registerDashboard path but keeping the limiter
// loose so burst tests don't spuriously trip.
func newScratchTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(&mockPlatform{})
	// registerDashboard already created the pool + handler; override the
	// rate limiter with a permissive one so open/close/promote cycles in
	// a single test don't hit the 5/min cap.
	if srv.scratchH == nil {
		t.Fatal("expected scratch handler to be wired by registerDashboard")
	}
	srv.scratchH.openLimit = nil
	return srv
}

func TestScratchOpen_Happy(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())

	body := `{"source_key":"feishu:direct:alice:general","quote":"what does the circuit breaker do?"}`
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ScratchID      string `json:"scratch_id"`
		Key            string `json:"key"`
		AgentID        string `json:"agent_id"`
		QuoteTruncated bool   `json:"quote_truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.ScratchID) != 32 {
		t.Errorf("scratch id length = %d", len(resp.ScratchID))
	}
	if !strings.HasPrefix(resp.Key, session.ScratchKeyPrefix) {
		t.Errorf("key missing prefix: %q", resp.Key)
	}
	if resp.QuoteTruncated {
		t.Error("unexpected truncation for short quote")
	}
}

func TestScratchOpen_MissingQuote(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open",
		strings.NewReader(`{"source_key":"feishu:direct:alice:general"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchOpen_UnknownSource(t *testing.T) {
	srv := newScratchTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open",
		strings.NewReader(`{"source_key":"no:such:chat:general","quote":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestScratchOpen_SourceIsScratchRefused(t *testing.T) {
	srv := newScratchTestServer(t)
	// Simulate a live scratch source.
	scratchKey := "scratch:aaaaaa:general:general"
	srv.router.InjectSession(scratchKey, session.NewTestProcess())

	body, _ := json.Marshal(map[string]string{"source_key": scratchKey, "quote": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchOpen_InvalidSourceKey(t *testing.T) {
	srv := newScratchTestServer(t)
	body := map[string]string{"source_key": "bad\x00key", "quote": "hi"}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchDelete_InvalidID(t *testing.T) {
	srv := newScratchTestServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/scratch/short", nil)
	req.SetPathValue("id", "short")
	w := httptest.NewRecorder()
	srv.scratchH.handleDelete(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestScratchDelete_UnknownReturns204(t *testing.T) {
	// Idempotent delete: unknown but well-formed ID returns 204 so a client
	// retry after sweeper pruning does not surface as an error.
	srv := newScratchTestServer(t)
	req := httptest.NewRequest(http.MethodDelete,
		"/api/scratch/00000000000000000000000000000000", nil)
	req.SetPathValue("id", "00000000000000000000000000000000")
	w := httptest.NewRecorder()
	srv.scratchH.handleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestScratchDelete_Known(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	sc, err := srv.scratchPool.Open(session.OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "hello",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/scratch/"+sc.ID, nil)
	req.SetPathValue("id", sc.ID)
	w := httptest.NewRecorder()
	srv.scratchH.handleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if srv.scratchPool.Get(sc.ID) != nil {
		t.Error("scratch still in pool after DELETE")
	}
}

func TestScratchPromote_RenamesSession(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	sc, err := srv.scratchPool.Open(session.OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "promote me",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The scratch key must be registered in the router for promote to
	// succeed — InjectSession handles this for a different key; here we
	// inject at sc.Key to simulate the first send having happened.
	srv.router.InjectSession(sc.Key, session.NewTestProcess())

	req := httptest.NewRequest(http.MethodPost, "/api/scratch/"+sc.ID+"/promote", nil)
	req.SetPathValue("id", sc.ID)
	w := httptest.NewRecorder()
	srv.scratchH.handlePromote(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Key, "feishu:direct:alice:aside-") {
		t.Errorf("promoted key shape unexpected: %q", resp.Key)
	}
	if srv.router.GetSession(resp.Key) == nil {
		t.Error("promoted session not found under new key")
	}
	if srv.router.GetSession(sc.Key) != nil {
		t.Error("old scratch key still present after promote")
	}
	if srv.scratchPool.Get(sc.ID) != nil {
		t.Error("pool should have detached scratch after promote")
	}
}

func TestScratchPromote_UnknownID(t *testing.T) {
	srv := newScratchTestServer(t)
	id := "00000000000000000000000000000000"
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/"+id+"/promote", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.scratchH.handlePromote(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestScratchListFilteredFromSessions(t *testing.T) {
	// handleList must hide scratch keys from the sidebar payload.
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	// 32-char scratch id (lowercase hex) matches the newScratchID shape.
	scratchKey := "scratch:cccccccccccccccccccccccccccccccc:general:general"
	srv.router.InjectSession(scratchKey, session.NewTestProcess())

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "scratch:") {
		t.Errorf("sidebar payload should not include scratch keys: %s", w.Body.String())
	}
}

// TestScratchOpen_InjectsSurroundingContext seeds the source session's
// event log with a handful of turns surrounding a quoted user message and
// verifies handleOpen pulls neighbours into the scratch via the new
// context fields. Drives the end-to-end path rather than unit testing
// the renderer in isolation.
func TestScratchOpen_InjectsSurroundingContext(t *testing.T) {
	srv := newScratchTestServer(t)
	proc := session.NewTestProcess()
	// Append 6 events; the 4th (t=4000) is the quoted user message.
	proc.EventLog.Append(cli.EventEntry{Time: 1000, Type: "user", Detail: "q1 before"})
	proc.EventLog.Append(cli.EventEntry{Time: 2000, Type: "text", Detail: "a1 before"})
	proc.EventLog.Append(cli.EventEntry{Time: 3000, Type: "tool_use", Tool: "Read", Summary: "noise"})
	proc.EventLog.Append(cli.EventEntry{Time: 4000, Type: "user", Detail: "the quoted question"})
	proc.EventLog.Append(cli.EventEntry{Time: 5000, Type: "text", Detail: "a2 after"})
	proc.EventLog.Append(cli.EventEntry{Time: 6000, Type: "user", Detail: "q2 after"})
	srv.router.InjectSession("feishu:direct:alice:general", proc)

	body := `{"source_key":"feishu:direct:alice:general","quote":"the quoted question","source_message_time":4000,"context_turns":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ScratchID        string `json:"scratch_id"`
		ContextTurns     int    `json:"context_turns"`
		ContextTruncated bool   `json:"context_truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Filtered eligible entries: t=1000 user, t=2000 text, t=5000 text,
	// t=6000 user — the tool_use at t=3000 and the quote itself at t=4000
	// are excluded. Expect all 4 to fit comfortably in the default budget.
	if resp.ContextTurns != 4 {
		t.Errorf("ContextTurns = %d, want 4", resp.ContextTurns)
	}
	if resp.ContextTruncated {
		t.Error("did not expect truncation with 4 small turns")
	}
	// Inspect the scratch's BaseOpts to confirm the rendered prompt.
	sc := srv.scratchPool.Get(resp.ScratchID)
	if sc == nil {
		t.Fatal("scratch missing from pool")
	}
	prompt := sc.BaseOpts.ExtraArgs[len(sc.BaseOpts.ExtraArgs)-1]
	for _, mustHave := range []string{"q1 before", "a1 before", "a2 after", "q2 after", "<conversation_context>", "<selected_quote>"} {
		if !strings.Contains(prompt, mustHave) {
			t.Errorf("prompt missing %q\n---\n%s", mustHave, prompt)
		}
	}
	for _, forbidden := range []string{"tool_use", "Read", "noise"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("prompt should not contain noise %q: %s", forbidden, prompt)
		}
	}
	// The quoted message itself must NOT be echoed inside the context block
	// (it is already carried in <selected_quote>).
	ctxStart := strings.Index(prompt, "<conversation_context>")
	ctxEnd := strings.Index(prompt, "</conversation_context>")
	if ctxStart < 0 || ctxEnd < 0 || ctxEnd < ctxStart {
		t.Fatalf("conversation_context markers malformed: %s", prompt)
	}
	ctxBody := prompt[ctxStart:ctxEnd]
	if strings.Contains(ctxBody, "the quoted question") {
		t.Errorf("quoted message leaked into context block: %s", ctxBody)
	}
}

// TestScratchOpen_TurnCountClamped prevents a client-supplied context_turns
// above MaxScratchContextTurns from escaping the clamp.
func TestScratchOpen_TurnCountClamped(t *testing.T) {
	srv := newScratchTestServer(t)
	proc := session.NewTestProcess()
	srv.router.InjectSession("feishu:direct:alice:general", proc)

	body := `{"source_key":"feishu:direct:alice:general","quote":"hi","context_turns":9999}`
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	// No events on the source, so ContextTurns should be 0, and the
	// over-sized request must not cause a panic / 500.
	var resp struct {
		ContextTurns int `json:"context_turns"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ContextTurns != 0 {
		t.Errorf("ContextTurns = %d, want 0 for empty source", resp.ContextTurns)
	}
}

// TestScratchOpen_NoTimestampFallback exercises the older-dashboard path
// where the client never sends source_message_time: collectScratchContext
// must fall back to EventLastN as the before-window and leave after empty.
// Locks down that operators on stale client builds still get *some*
// surrounding context rather than a bare <selected_quote>.
func TestScratchOpen_NoTimestampFallback(t *testing.T) {
	srv := newScratchTestServer(t)
	proc := session.NewTestProcess()
	// Six events across the log; no single one is marked as the quote
	// because we are exercising the timestamp-less path.
	proc.EventLog.Append(cli.EventEntry{Time: 1000, Type: "user", Detail: "old-q1"})
	proc.EventLog.Append(cli.EventEntry{Time: 2000, Type: "text", Detail: "old-a1"})
	proc.EventLog.Append(cli.EventEntry{Time: 3000, Type: "tool_use", Tool: "Read", Summary: "noise"})
	proc.EventLog.Append(cli.EventEntry{Time: 4000, Type: "user", Detail: "new-q2"})
	proc.EventLog.Append(cli.EventEntry{Time: 5000, Type: "text", Detail: "new-a2"})
	srv.router.InjectSession("feishu:direct:alice:general", proc)

	// No source_message_time field → server treats as 0 and takes the tail.
	body := `{"source_key":"feishu:direct:alice:general","quote":"asking about prior conversation"}`
	req := httptest.NewRequest(http.MethodPost, "/api/scratch/open", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.scratchH.handleOpen(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ScratchID    string `json:"scratch_id"`
		ContextTurns int    `json:"context_turns"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Filtered eligible tail: user/text entries only → 4 turns survive the
	// noise filter (tool_use at t=3000 dropped). Default budget fits all.
	if resp.ContextTurns != 4 {
		t.Errorf("ContextTurns = %d, want 4 (tail-window fallback)", resp.ContextTurns)
	}
	sc := srv.scratchPool.Get(resp.ScratchID)
	if sc == nil {
		t.Fatal("scratch missing from pool")
	}
	prompt := sc.BaseOpts.ExtraArgs[len(sc.BaseOpts.ExtraArgs)-1]
	// All 4 tail-window turns must be present in the rendered block.
	for _, mustHave := range []string{"old-q1", "old-a1", "new-q2", "new-a2"} {
		if !strings.Contains(prompt, mustHave) {
			t.Errorf("prompt missing tail-window entry %q", mustHave)
		}
	}
	if strings.Contains(prompt, "noise") {
		t.Error("tool_use entry leaked into tail-window context")
	}
}

// Sanity check that the sweeper is started as part of registerDashboard
// and actually drops an idle scratch. Backdates lastUsed then triggers
// sweep manually via exposed test seam.
func TestScratchSweeperRegistered(t *testing.T) {
	srv := newScratchTestServer(t)
	srv.router.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	sc, _ := srv.scratchPool.Open(session.OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "fade me",
	})
	// Force-expire by back-dating; we don't wait for the real tick here.
	srv.scratchPool.ForceExpireForTest(sc.ID, time.Now().Add(-2*session.DefaultScratchTTL))
	srv.scratchPool.SweepForTest(time.Now())
	if srv.scratchPool.Get(sc.ID) != nil {
		t.Error("sweeper should have dropped expired scratch")
	}
}
