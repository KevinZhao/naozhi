package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// ─── handleAPISessionEvents ──────────────────────────────────────────────────

func TestHandleAPISessionEvents_MissingKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing key") {
		t.Errorf("body = %q, want 'missing key'", w.Body.String())
	}
}

func TestHandleAPISessionEvents_SessionNotFound(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key=no-such-key", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session not found") {
		t.Errorf("body = %q, want 'session not found'", w.Body.String())
	}
}

// TestSessionHandlers_RejectInvalidKey covers R172-SEC-L2 / R175-SEC-M: the
// events, delete (body and query), label and interrupt handlers each run
// sessionkey.ValidateSessionKey (its cases are tested there), so a key with an
// embedded control byte is a 400 before it reaches the router or slog.
func TestSessionHandlers_RejectInvalidKey(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	const key = "test:direct:abc\x00def:general"
	body := func(extra string) io.Reader {
		m := map[string]string{"key": key}
		if extra != "" {
			m["label"] = extra
		}
		raw, _ := json.Marshal(m)
		return bytes.NewReader(raw)
	}
	h := srv.sessionH
	for name, call := range map[string]func(w http.ResponseWriter){
		"events": func(w http.ResponseWriter) {
			h.HandleEvents(w, httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+url.QueryEscape(key), nil))
		},
		"delete_body": func(w http.ResponseWriter) {
			h.HandleDelete(w, httptest.NewRequest(http.MethodDelete, "/api/sessions", body("")))
		},
		"delete_query": func(w http.ResponseWriter) {
			h.HandleDelete(w, httptest.NewRequest(http.MethodDelete, "/api/sessions?key="+url.QueryEscape(key), nil))
		},
		"set_label": func(w http.ResponseWriter) {
			h.HandleSetLabel(w, httptest.NewRequest(http.MethodPatch, "/api/sessions/label", body("x")))
		},
		"interrupt": func(w http.ResponseWriter) {
			h.HandleInterrupt(w, httptest.NewRequest(http.MethodPost, "/api/sessions/interrupt", body("")))
		},
	} {
		w := httptest.NewRecorder()
		call(w)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid key") {
			t.Errorf("%s: status = %d body = %q, want 400 invalid key", name, w.Code, w.Body.String())
		}
	}
}

// seedEventSession injects a session with the given entries and returns its key.
// Extracted so pagination tests can share setup.
func seedEventSession(t *testing.T, srv *Server, times ...int64) string {
	t.Helper()
	evs := make([]typedEvent, len(times))
	for i, ts := range times {
		evs[i] = typedEvent{ts, "text"}
	}
	return seedTypedEventSession(t, srv, evs...)
}

// typedEvent is a (time,type) pair for seedTypedEventSession.
type typedEvent struct {
	time int64
	typ  string
}

// seedTypedEventSession injects a session whose event log carries the given
// typed entries (in order). Used to exercise the visible-aware initial read
// where the type — not just the timestamp — decides what the dashboard renders.
func seedTypedEventSession(t *testing.T, srv *Server, events ...typedEvent) string {
	t.Helper()
	key := "test:d:u:general"
	proc := session.NewTestProcess()
	for _, e := range events {
		proc.EventLog.Append(clievent.EventEntry{Time: e.time, Type: e.typ, Summary: "x"})
	}
	routerOf(srv).InjectSession(key, proc)
	return key
}

// TestHandleAPISessionEvents_InitialPageIsVisibleAware reproduces the
// "parallel agent team ate my history" bug at the HTTP layer. The trailing
// `limit` events are all internal (task_progress); a plain tail-N would return
// a page the dashboard filters to nothing and renders blank. The visible-aware
// initial read (beforeStr=="" && limit>0) must keep walking back so the page
// carries the real "text" bubbles.
func TestHandleAPISessionEvents_InitialPageIsVisibleAware(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	var evs []typedEvent
	ts := int64(0)
	// 3 visible messages up front, then a 200-event internal flood at the tail.
	for m := 0; m < 3; m++ {
		ts++
		evs = append(evs, typedEvent{ts, "text"})
	}
	for i := 0; i < 200; i++ {
		ts++
		evs = append(evs, typedEvent{ts, "task_progress"})
	}
	key := seedTypedEventSession(t, srv, evs...)

	// Initial fetch (limit only, no before) with limit=100 — the trailing 100
	// are all internal.
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&limit=100", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []clievent.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	visible := 0
	for _, e := range entries {
		if e.Type == "text" {
			visible++
		}
	}
	if visible < 3 {
		t.Errorf("initial page carried %d visible bubbles, want >=3 — visible-aware read should walk past the internal flood (got %d entries total)", visible, len(entries))
	}
}

// TestHandleAPISessionEvents_BeforeStaysTimeOrdered guards the regression
// boundary: the "load earlier" pagination path (before>0) must NOT switch to
// the visible-aware reader, or it would skip past internal events the operator
// is paging toward. before=250 must return the contiguous time-ordered slice,
// internal events included.
func TestHandleAPISessionEvents_BeforeStaysTimeOrdered(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	var evs []typedEvent
	for ts := int64(1); ts <= 300; ts++ {
		typ := "task_progress"
		if ts%50 == 0 {
			typ = "text"
		}
		evs = append(evs, typedEvent{ts, typ})
	}
	key := seedTypedEventSession(t, srv, evs...)

	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&before=250&limit=10", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []clievent.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 10 {
		t.Fatalf("len = %d, want 10 (plain time-ordered page)", len(entries))
	}
	// The newest entry strictly older than 250 is time 249; the page is the
	// 10 entries 240..249, mostly internal — proving no visible-aware skipping.
	if entries[len(entries)-1].Time != 249 {
		t.Errorf("newest entry Time = %d, want 249 (contiguous backward page)", entries[len(entries)-1].Time)
	}
	if entries[0].Time != 240 {
		t.Errorf("oldest entry Time = %d, want 240", entries[0].Time)
	}
}

// TestHandleAPISessionEvents_InitialPageHasMoreHeader is the regression for the
// stranded-opening-messages bug. The session has more visible bubbles than the
// initial page carries (server truncates by DefaultVisibleTarget), so older
// history exists and the X-Events-Has-More header must be "1" — even when the
// total returned event count is below the client's limit, which is exactly the
// case the old client-side len>=limit heuristic missed.
func TestHandleAPISessionEvents_InitialPageHasMoreHeader(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	var evs []typedEvent
	// 5 * DefaultVisibleTarget visible messages, all text — far more visible
	// bubbles than DefaultVisibleTarget, so the initial page can't include the
	// oldest ones.
	n := 5 * session.DefaultVisibleTarget
	for ts := int64(1); ts <= int64(n); ts++ {
		evs = append(evs, typedEvent{ts, "text"})
	}
	key := seedTypedEventSession(t, srv, evs...)

	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&limit=100", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Events-Has-More"); got != "1" {
		t.Errorf("X-Events-Has-More = %q, want \"1\" (older history exists beyond the initial page)", got)
	}
}

// TestHandleAPISessionEvents_Paging: limit keeps the newest entries, before
// pages strictly older ones (newest first when both cap), and a limit past
// the server's max is clamped rather than refused. Both the initial and the
// before= branch always set X-Events-Has-More, so the client need not read a
// short page as exhausted (nor show a useless "load earlier" on a full one).
func TestHandleAPISessionEvents_Paging(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000, 2000, 3000, 4000, 5000)
	for _, tc := range []struct {
		query string
		want  []int64
		more  string
	}{
		{"&limit=2", []int64{4000, 5000}, "1"},
		{"&before=3500&limit=10", []int64{1000, 2000, 3000}, "0"},
		{"&before=3500&limit=2", []int64{2000, 3000}, "1"},
		{"&before=5000&limit=2", []int64{3000, 4000}, "1"},
		{"&limit=10000", []int64{1000, 2000, 3000, 4000, 5000}, "0"},
	} {
		w := httptest.NewRecorder()
		srv.sessionH.HandleEvents(w, httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+key+tc.query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.query, w.Code)
		}
		if got := w.Header().Get("X-Events-Has-More"); got != tc.more {
			t.Errorf("%s: X-Events-Has-More = %q, want %q", tc.query, got, tc.more)
		}
		var entries []clievent.EventEntry
		if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
			t.Fatalf("%s: decode: %v", tc.query, err)
		}
		var got []int64
		for _, e := range entries {
			got = append(got, e.Time)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: times = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// fakeEventsSource is a history.Source stub for the pagination fallback test.
// Using a local stub avoids cross-package test imports and keeps the disk-tier
// expectations self-contained.
type fakeEventsSource struct {
	entries []clievent.EventEntry
	called  int
}

// LoadBefore implements history.Source over the chronological entries: the
// newest limit with Time < beforeMS, so the has-more probe sees a real cursor.
func (f *fakeEventsSource) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	f.called++
	var out []clievent.EventEntry
	for _, e := range f.entries {
		if beforeMS <= 0 || e.Time < beforeMS {
			out = append(out, e)
		}
	}
	if limit <= 0 {
		return nil, nil
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// TestHandleAPISessionEvents_BeforeFallsBackToHistorySource pins the contract
// that the dashboard pagination endpoint consults the session's history.Source
// once the in-memory ring has nothing strictly older than `before`. This is
// the whole point of the disk-tier refactor: long chats whose JSONL carries
// more than the 500-entry in-memory cap must still paginate backwards past
// the cap, rather than dead-ending at "no more events".
func TestHandleAPISessionEvents_BeforeFallsBackToHistorySource(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	// Memory has entries 1000, 2000, 3000; Source holds hypothetical older
	// entries at 100, 200 that no longer fit in the live ring.
	key := seedEventSession(t, srv, 1000, 2000, 3000)
	sess := srv.router.SessionFor(key)
	if sess == nil {
		t.Fatalf("session %q not registered", key)
	}
	src := &fakeEventsSource{entries: []clievent.EventEntry{
		{Time: 100, Type: "text", Summary: "ancient-1"},
		{Time: 200, Type: "text", Summary: "ancient-2"},
	}}
	sess.SetHistorySource(src)

	// before=500 — strictly older than every memory entry; memory tier
	// returns empty and the handler falls back to the Source.
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&before=500&limit=10", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []clievent.EventEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// One page read plus the has-more probe below the page's oldest entry.
	if src.called != 2 {
		t.Errorf("history.Source calls = %d, want 2", src.called)
	}
	if got := w.Header().Get("X-Events-Has-More"); got != "0" {
		t.Errorf("X-Events-Has-More = %q, want \"0\" (disk exhausted at the page bottom)", got)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Summary != "ancient-1" || entries[1].Summary != "ancient-2" {
		t.Errorf("entries = %+v, want [ancient-1, ancient-2]", entries)
	}
}

// TestHandleAPISessionEvents_BeforeSkipsSourceWhenMemoryCovers pins the
// inverse: when memory fills the whole page, the Source must not be
// consulted. Preserves the hot-path invariant that the first N pages of
// "load earlier" don't incur disk I/O: memory also answers the has-more
// probe while it holds an entry below the page. (A short memory page is the
// memory bottom and is topped up from the Source — see
// TestEventEntriesBeforeCtx_TopsUpShortMemoryPageFromSource.)
func TestHandleAPISessionEvents_BeforeSkipsSourceWhenMemoryCovers(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 500, 1000, 2000, 3000)
	sess := srv.router.SessionFor(key)
	if sess == nil {
		t.Fatalf("session %q not registered", key)
	}
	src := &fakeEventsSource{entries: []clievent.EventEntry{{Time: 100, Summary: "ancient"}}}
	sess.SetHistorySource(src)

	// before=2500&limit=2 — memory's {1000, 2000} fill the page; no need for Source.
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions/events?key="+key+"&before=2500&limit=2", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if src.called != 0 {
		t.Errorf("memory hit must not consult Source, got %d calls", src.called)
	}
	if got := w.Header().Get("X-Events-Has-More"); got != "1" {
		t.Errorf("X-Events-Has-More = %q, want \"1\" (500 is below the page)", got)
	}
}

func TestHandleAPISessionEvents_InvalidParams(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	seedEventSession(t, srv, 1000)

	for _, q := range []string{"before=not-a-number", "limit=abc", "limit=-1"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/sessions/events?key=test:d:u:general&"+q, nil)
		w := httptest.NewRecorder()
		srv.sessionH.HandleEvents(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, w.Code)
		}
	}
}

// ─── handleAPISend ────────────────────────────────────────────────────────────

// TestHandleAPISend_Rejects: the JSON send path refuses a missing key, a text
// over the WS path's per-field cap (R60-SEC-2), a key with a control byte
// before any slog attr is written (R60-SEC-8), a send with neither text nor
// files, and a malformed body.
func TestHandleAPISend_Rejects(t *testing.T) {
	_, hs := newTestServerHS(&mockPlatform{})
	for _, tc := range []struct{ body, want string }{
		{`{"text":"hello"}`, "key is required"},
		{`{"key":"p:t:u:general","text":"` + strings.Repeat("x", maxWSSendTextBytes+1) + `"}`, "too long"},
		{`{"key":"p:t:u\nadmin:general","text":"hi"}`, "invalid key"},
		{`{"key":"p:t:u:general"}`, "text or files"},
		{"{bad json", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/send", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		hs.sendH.handleSend(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%.40s: status = %d body = %q, want 400 %q", tc.body, w.Code, w.Body.String(), tc.want)
		}
	}
}

func TestHandleAPISend_AcceptedWithValidToken(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status = %q, want accepted", resp["status"])
	}
	if resp["key"] != "p:t:u:general" {
		t.Errorf("key = %q, want p:t:u:general", resp["key"])
	}
}

func TestHandleAPISend_QueuedWhenBusy(t *testing.T) {
	// A queue with room, as config's default max_depth gives production;
	// newTestServerHS's zero QueueOptions turn queueing off.
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:    ":0",
		Router:  session.NewRouter(session.RouterConfig{}),
		Backend: "claude",
		Queue:   QueueOptions{MaxDepth: 5},
	})
	t.Cleanup(srv.appCancel)
	key := "p:t:u:general"

	// Another send owns the key's turn, so this one queues behind it.
	busyForTest(t, srv.hub, key)

	body := `{"key":"p:t:u:general","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleSend(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "queued" {
		t.Errorf("status = %q, want 'queued'", resp["status"])
	}
}

// ─── handleAPISessions: stats include agents and workspace ──────────────────

func TestHandleAPISessions_StatsIncludeAgentsAndWorkspace(t *testing.T) {
	agents := map[string]session.AgentOpts{
		"code-reviewer": {Model: "sonnet"},
		"researcher":    {Model: "opus"},
	}
	router := session.NewRouter(session.RouterConfig{
		MaxProcs:  5,
		Workspace: "/test/workspace",
	})
	srv, _ := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Routing: RoutingOptions{Agents: agents}, Backend: "claude"})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	stats, ok := resp["stats"].(map[string]any)
	if !ok {
		t.Fatal("stats field missing")
	}

	// Check max_procs
	if mp, ok := stats["max_procs"].(float64); !ok || int(mp) != 5 {
		t.Errorf("max_procs = %v, want 5", stats["max_procs"])
	}

	// Check default_workspace
	if ws, ok := stats["default_workspace"].(string); !ok || ws != "/test/workspace" {
		t.Errorf("default_workspace = %v, want /test/workspace", stats["default_workspace"])
	}

	// Check agents list includes "general" and configured agents
	agentsList, ok := stats["agents"].([]any)
	if !ok {
		t.Fatal("agents field missing or not array")
	}
	agentSet := make(map[string]bool)
	for _, a := range agentsList {
		agentSet[a.(string)] = true
	}
	if !agentSet["general"] {
		t.Error("agents should include 'general'")
	}
	if !agentSet["code-reviewer"] {
		t.Error("agents should include 'code-reviewer'")
	}
	if !agentSet["researcher"] {
		t.Error("agents should include 'researcher'")
	}
}

// ─── handleAPISend: workspace override ──────────────────────────────────────

func TestHandleAPISend_WorkspaceOverride(t *testing.T) {
	tmpDir := t.TempDir()
	router := session.NewRouter(session.RouterConfig{
		Workspace: "/default/workspace",
	})
	_, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})

	key := "dashboard:direct:test-session:general"
	body := `{"key":"` + key + `","text":"hi","workspace":"` + tmpDir + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleSend(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	// Verify workspace was set for the chat key. validateWorkspace calls
	// filepath.EvalSymlinks, which on macOS rewrites /var/folders/... to
	// /private/var/folders/... and /tmp/... to /private/tmp/... — resolve
	// the expected value the same way so the assertion is platform-neutral.
	chatKey := "dashboard:direct:test-session"
	ws := router.Workspace(chatKey)
	wantDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", tmpDir, err)
	}
	if ws != wantDir {
		t.Errorf("workspace = %q, want %q", ws, wantDir)
	}
}

func TestHandleAPISend_WorkspaceInvalidDir(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{
		Workspace: "/default/workspace",
	})
	_, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})

	key := "dashboard:direct:test-session:general"
	body := `{"key":"` + key + `","text":"hi","workspace":"/nonexistent/path/xyz"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/send",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hs.sendH.handleSend(w, req)

	// Invalid workspace should be rejected with 403
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	// Verify workspace was NOT set
	chatKey := "dashboard:direct:test-session"
	ws := router.Workspace(chatKey)
	if ws != "/default/workspace" {
		t.Errorf("workspace = %q, want /default/workspace (invalid path should be rejected)", ws)
	}
}

// ─── multi-node API routing ──────────────────────────────────────────────────

// TestHandleAPISessions_NodeAggregation verifies that /api/sessions merges remote
// node sessions (from cache) and includes the nodes status map.
func TestHandleAPISessions_NodeAggregation(t *testing.T) {
	// Mock remote node serving /api/sessions with one session
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sessions":
			json.NewEncoder(w).Encode(map[string]any{
				"sessions": []map[string]any{
					{"key": "feishu:direct:bob:general", "state": "ready"},
				},
			})
		case "/api/projects":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/api/discovered":
			json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes.Add("macbook", node.NewHTTPClient("macbook", remote.URL, "", "MacBook Pro"))
	srv.nodes.SetKnown("macbook", "MacBook Pro")

	// Populate the cache synchronously
	srv.nodeCache.RefreshAll()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// nodes map must include macbook
	nodes, ok := resp["nodes"].(map[string]any)
	if !ok {
		t.Fatal("nodes field missing")
	}
	if _, ok := nodes["macbook"]; !ok {
		t.Error("nodes should contain 'macbook'")
	}
	if macbook, ok := nodes["macbook"].(map[string]any); ok {
		if macbook["status"] != "ok" {
			t.Errorf("macbook status = %v, want ok", macbook["status"])
		}
		if macbook["display_name"] != "MacBook Pro" {
			t.Errorf("macbook display_name = %v, want 'MacBook Pro'", macbook["display_name"])
		}
	}

	// sessions must include the remote session with node="macbook"
	sessions, ok := resp["sessions"].([]any)
	if !ok {
		t.Fatal("sessions field missing")
	}
	var found bool
	for _, s := range sessions {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if sm["key"] == "feishu:direct:bob:general" && sm["node"] == "macbook" {
			found = true
			break
		}
	}
	if !found {
		t.Error("remote session with node='macbook' not found in aggregated sessions")
	}
}

// ─── handleSetLabel (PATCH /api/sessions/label) ──────────────────────────────

// TestHandleSetLabel_OK verifies the happy path: an existing session accepts
// a label update and the mutation is visible via Router.GetSession.
func TestHandleSetLabel_OK(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)

	body := `{"key":"` + key + `","label":"重构会话"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.HandleSetLabel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if got := srv.router.SessionFor(key).UserLabel(); got != "重构会话" {
		t.Errorf("router UserLabel = %q, want 重构会话", got)
	}
}

// TestHandleSetLabel_EmptyClears verifies that an empty label clears any
// prior label (the documented "reset to auto-title" gesture).
func TestHandleSetLabel_EmptyClears(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)
	routerOf(srv).SetUserLabel(key, "before")

	body := `{"key":"` + key + `","label":""}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.HandleSetLabel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := srv.router.SessionFor(key).UserLabel(); got != "" {
		t.Errorf("UserLabel = %q, want empty after clear", got)
	}
}

// TestHandleSetLabel_Rejects: a label TestValidateUserLabel refuses (too
// long, a control byte) and a missing key are 400s; an unknown key is a 404.
func TestHandleSetLabel_Rejects(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := seedEventSession(t, srv, 1000)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"key":"` + key + `","label":"` + strings.Repeat("a", sessionview.MaxUserLabelBytes+1) + `"}`, http.StatusBadRequest},
		{`{"key":"` + key + `","label":"line1\nline2"}`, http.StatusBadRequest},
		{`{"label":"x"}`, http.StatusBadRequest},
		{`{"key":"no:such:key","label":"x"}`, http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		srv.sessionH.HandleSetLabel(w, httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Errorf("%.60s: status = %d, want %d", tc.body, w.Code, tc.want)
		}
	}
}

// TestHandleSetLabel_RemoteProxy verifies that a node-scoped request is
// forwarded to the remote's PATCH /api/sessions/label endpoint.
func TestHandleSetLabel_RemoteProxy(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   string
	)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "label": "remote-label"})
	}))
	defer remote.Close()

	srv := newTestServer(&mockPlatform{})
	srv.nodes.Add("macbook", node.NewHTTPClient("macbook", remote.URL, "", "MacBook Pro"))
	srv.nodes.SetKnown("macbook", "MacBook Pro")

	body := `{"key":"feishu:direct:alice:general","node":"macbook","label":"remote-label"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sessions/label", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.sessionH.HandleSetLabel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("remote method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/api/sessions/label" {
		t.Errorf("remote path = %q, want /api/sessions/label", gotPath)
	}
	if !strings.Contains(gotBody, "remote-label") {
		t.Errorf("remote body = %q, want it to contain 'remote-label'", gotBody)
	}
}

// ─── handleResume last_prompt charset (R65-SEC-M-3) ──────────────────────────

// TestSanitizeResumeLastPrompt_RuneSafeTruncation pins R217's rune-boundary
// truncation: when len(mapped) > maxLen, we must NOT split a multi-byte
// codepoint mid-sequence, otherwise sessions.json / dashboard surfaces the
// invalid UTF-8 as a replacement glyph.
func TestSanitizeResumeLastPrompt_RuneSafeTruncation(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		// "你好" (3 bytes each in UTF-8). maxLen=3 should keep one rune,
		// not "你" + first byte of "好" (which would be invalid UTF-8).
		{"truncate at first rune boundary", "你好", 3, "你"},
		// maxLen falls between a rune; textutil.TruncateAtRuneBoundary walks
		// back to the most recent valid boundary.
		{"truncate before second rune", "你好", 4, "你"},
		// ASCII-only: byte == rune, no walkback needed.
		{"ascii truncate", "abcdef", 3, "abc"},
		// maxLen large enough to fit the whole string: no truncation.
		{"no truncation needed", "你好", 10, "你好"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Force the sanitize-then-truncate path by including a control
			// char that strings.Map will replace, ensuring we exit the
			// `clean` short-circuit. Then strip the inserted control to
			// get the rune-boundary-truncation result.
			input := "\x00" + tc.in
			want := "_" + tc.want
			// textutil.TruncateAtRuneBoundary returns up to maxLen bytes ending
			// at a rune boundary; for a sentinel "_" + body the cap is len(want).
			got := dashsession.SanitizeResumeLastPrompt(input, len(want))
			if got != want {
				t.Errorf("dashsession.SanitizeResumeLastPrompt(%q, %d) = %q, want %q",
					input, len(want), got, want)
			}
			// Result must be valid UTF-8 even on the truncation branch.
			if !utf8.ValidString(got) {
				t.Errorf("result %q is not valid UTF-8", got)
			}
		})
	}
}

// TestSanitizeResumeLastPrompt_Policy pins the rune-level policy so a future
// refactor reaches consensus via this test. Tab is preserved; C0/DEL/C1/bidi/
// LS/PS are all replaced with "_".
func TestSanitizeResumeLastPrompt_Policy(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean ascii", "hello world", "hello world"},
		{"tab preserved", "col1\tcol2", "col1\tcol2"},
		{"C0 NUL", "a\x00b", "a_b"},
		{"C0 BS", "a\x08b", "a_b"},
		{"DEL", "a\x7fb", "a_b"},
		{"C1 NEL U+0085", "a\u0085b", "a_b"},
		{"bidi RLO U+202E", "a\u202eb", "a_b"},
		{"LS U+2028", "a\u2028b", "a_b"},
		{"CJK kept", "\u4f60\u597d", "\u4f60\u597d"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := dashsession.SanitizeResumeLastPrompt(tc.in, 0)
			if got != tc.want {
				t.Errorf("dashsession.SanitizeResumeLastPrompt(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ─── R67 regressions ─────────────────────────────────────────────────────────

// TestHandlePreview_RejectsInvalidNodeID locks down R67-SEC-2: the preview
// handler must run `nodeID` through LookupNode (which enforces the
// [a-zA-Z0-9._-] allowlist) before logging or proxying. Before the fix,
// `GetNode(nodeID)` passed raw nodeID strings — including newlines and
// ANSI escapes — into the subsequent slog.Warn attr, breaking log parsing.
func TestHandlePreview_RejectsInvalidNodeID(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	// Valid session_id shape (length + charset match IsValidSessionID) but
	// malicious nodeID containing a newline — MUST be rejected at the
	// LookupNode boundary with 400, not fall through to the slog.Warn path.
	u := "/api/discovered/preview?session_id=12345678-1234-1234-1234-123456789abc&node=bad%0Anode"
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.discoveryH.HandlePreview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%q)", w.Code, w.Body.String())
	}
}

// writePreviewJSONL drops a minimal one-line user JSONL at the CWD-derived
// path under claudeDir so HandlePreview's local branch can resolve it. Returns
// the session id it wrote.
func writePreviewJSONL(t *testing.T, claudeDir, cwd, content string) string {
	t.Helper()
	sessionID := "12345678-1234-1234-1234-123456789abc"
	projDir := filepath.Join(claudeDir, "projects", claudefs.ProjectSlug(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":` +
		mustJSON(t, content) + `}}`
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHandlePreview_CWDHintResolvesContent locks down the preview-flake fix:
// a valid `cwd` query param lets the handler resolve the JSONL via the O(1)
// CWD-derived path, returning the session content. This is the path that
// bypasses findSessionJSONL's 60s negative cache — the cache that made
// interactive preview intermittently render a blank "暂无会话历史" splash.
func TestHandlePreview_CWDHintResolvesContent(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	claudeDir := t.TempDir()
	srv.discoveryH.SetClaudeDirForTest(claudeDir)

	cwd := filepath.Join(t.TempDir(), "preview-proj")
	sessionID := writePreviewJSONL(t, claudeDir, cwd, "hello preview")

	u := "/api/discovered/preview?session_id=" + sessionID + "&cwd=" + url.QueryEscape(cwd)
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.discoveryH.HandlePreview(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	var entries []clievent.EventEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode body: %v (body=%q)", err, w.Body.String())
	}
	if len(entries) != 1 || entries[0].Summary != "hello preview" {
		t.Fatalf("entries = %+v, want one entry summarised %q", entries, "hello preview")
	}
}

// TestHandlePreview_InvalidCWDDegradesGracefully confirms a hostile cwd hint
// (path traversal) is dropped to "" rather than erroring or being used to
// probe arbitrary projDirName slugs. The handler still returns 200 with an
// empty list (no JSONL resolvable without the real cwd), never a 4xx/5xx.
func TestHandlePreview_InvalidCWDDegradesGracefully(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.discoveryH.SetClaudeDirForTest(t.TempDir())

	u := "/api/discovered/preview?session_id=12345678-1234-1234-1234-123456789abc&cwd=" +
		url.QueryEscape("/home/../etc")
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.discoveryH.HandlePreview(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (invalid cwd must degrade, not error; body=%q)", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("body = %q, want empty array", w.Body.String())
	}
}

// TestHandleTakeover_RejectsTraversalCWD locks down R67-SEC-7: `..`
// segments in the takeover CWD must be rejected before filepath.Clean
// collapses them. Previously, `/home/../etc` silently Clean'd to `/etc`
// and, when `allowedRoot == ""`, the CLI was spawned in the wrong
// directory.
func TestHandleTakeover_RejectsTraversalCWD(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	// Inject a non-empty claudeDir so the R67-SEC-4 "discovery not
	// available" early return does not fire; we're targeting the CWD
	// gate specifically. Using t.TempDir() to avoid touching real state.
	srv.discoveryH.SetClaudeDirForTest(t.TempDir())

	body := `{"pid":99999,"session_id":"12345678-1234-1234-1234-123456789abc","cwd":"/home/../etc","proc_start_time":1234}`
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.discoveryH.HandleTakeover(w, req)

	// Without the fix, this traversal would either pass (allowedRoot empty)
	// or be rejected much later. The fix returns 400 at the validation gate
	// before any kill attempt.
	if w.Code == http.StatusOK || w.Code == http.StatusAccepted {
		t.Errorf("traversal CWD accepted (status=%d) — validateRemoteWorkspace did not fire", w.Code)
	}
}

// TestHandleTakeover_NoClaudeDirRefuses locks down R67-SEC-4: when
// claudeDir is unavailable there is no discovered list to cross-check
// against, so any pid+start_time accepts arbitrary SIGTERM targets. The
// handler must return 503 like handleClose does.
func TestHandleTakeover_NoClaudeDirRefuses(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.discoveryH.SetClaudeDirForTest("") // simulate "discovery not available"

	body := `{"pid":1,"session_id":"12345678-1234-1234-1234-123456789abc","cwd":"/tmp","proc_start_time":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/discovered/takeover", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.discoveryH.HandleTakeover(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body=%q)", w.Code, w.Body.String())
	}
}

// TestHistorySessions_EmptyHistoryCached locks down R67-GO-5: when the
// underlying FS has no history entries, loadHistorySessions stores a
// legitimate (nil, now()) cache tuple; the singleflight double-check must
// treat this as "cached" and NOT re-run the scan on subsequent calls. The
// prior `historyCache != nil` guard misclassified empty-history results
// as "not cached" and drove a redundant FS scan per TTL window.
func TestHistorySessions_EmptyHistoryCached(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	// Wait for the WarmHistoryCache goroutine that New() launches before
	// mutating claudeDir — otherwise the warm goroutine reads the field
	// while the test writes it and -race trips. The warm goroutine uses
	// the runner's real ~/.claude so its result is discarded below by
	// forcing historyCacheTime=zero; we only need to sync with it.
	srv.sessionH.WaitWarmHistory()

	// Reset cache from the warm pass so the first test call is a real miss.
	// Mirror the atomic alongside the time.Time so the wait-free fast path
	// in historySessions() observes "expired" too. R040034-PERF-5 (#1404).
	srv.sessionH.ResetHistoryCacheForTest()

	// Empty claudeDir so loadHistorySessions naturally yields nil.
	srv.sessionH.SetClaudeDirForTest(t.TempDir())

	// First call: miss → loadHistorySessions → stores (nil, now()).
	_ = srv.sessionH.HistorySessionsForTest()

	// Capture the cache time — the whole point of the fix is that it is
	// NON-ZERO after a successful load of empty history.
	cacheTimeAfterFirst := srv.sessionH.HistoryCacheTimeForTest()
	if cacheTimeAfterFirst.IsZero() {
		t.Fatal("historyCacheTime is zero after load — loadHistorySessions did not store the sentinel")
	}

	// Second immediate call: hit → must NOT update historyCacheTime
	// because it is still within the 120s TTL window.
	_ = srv.sessionH.HistorySessionsForTest()

	cacheTimeAfterSecond := srv.sessionH.HistoryCacheTimeForTest()
	if !cacheTimeAfterSecond.Equal(cacheTimeAfterFirst) {
		t.Errorf("empty-history cache was re-loaded — cacheTime changed from %v to %v", cacheTimeAfterFirst, cacheTimeAfterSecond)
	}
}

// TestHandleDelete_AcceptsQueryAndBody pins the dual-input contract added
// in Round 120: DELETE /api/sessions must accept the key via either ?key=
// query string (REST-idiomatic, works for curl -X DELETE URL?key=...)
// or JSON body (legacy; dashboard.js still uses this).
//
// Each case exercises the handler with a router that holds no sessions,
// so a correctly-parsed key reaches h.router.Remove() and surfaces the
// "session not found" 404 — a parse failure would instead surface a
// 400 "key is required". The 400 vs 404 discriminator makes the test
// trivially robust against refactors that don't touch the parse logic.
func TestHandleDelete_AcceptsQueryAndBody(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{MaxProcs: 5})
	srv, _ := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})

	cases := []struct {
		name       string
		url        string
		body       string
		wantStatus int
		wantBody   string // substring match; "" = no body check
	}{
		{
			name:       "query-only",
			url:        "/api/sessions?key=missing",
			body:       "",
			wantStatus: http.StatusNotFound,
			wantBody:   "session not found",
		},
		{
			name:       "body-only (legacy)",
			url:        "/api/sessions",
			body:       `{"key":"missing"}`,
			wantStatus: http.StatusNotFound,
			wantBody:   "session not found",
		},
		{
			name:       "no key anywhere → 400",
			url:        "/api/sessions",
			body:       "",
			wantStatus: http.StatusBadRequest,
			wantBody:   "key is required",
		},
		{
			name:       "malformed JSON, no query → 400",
			url:        "/api/sessions",
			body:       `not-json`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "key is required",
		},
		{
			name:       "empty JSON body, no query → 400",
			url:        "/api/sessions",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "key is required",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(http.MethodDelete, tc.url, body)
			w := httptest.NewRecorder()
			srv.sessionH.HandleDelete(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want to contain %q", w.Body.String(), tc.wantBody)
			}
		})
	}
}
