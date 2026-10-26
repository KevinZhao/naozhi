package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ─── mock platform ────────────────────────────────────────────────────────────

type mockPlatform struct {
	mu      sync.Mutex
	replies []platform.OutgoingMessage
}

func (m *mockPlatform) Name() string                                               { return "test" }
func (m *mockPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (m *mockPlatform) EditMessage(_ context.Context, _ string, _ string) error    { return nil }

func (m *mockPlatform) MaxReplyLength() int { return 4000 }

func (m *mockPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies = append(m.replies, msg)
	return "msg-id", nil
}

func (m *mockPlatform) replyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.replies)
}

func (m *mockPlatform) allReplies() []platform.OutgoingMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]platform.OutgoingMessage, len(m.replies))
	copy(out, m.replies)
	return out
}

// ─── test helpers ─────────────────────────────────────────────────────────────

// newTestServerHS is newTestServer plus the handlerSet. #2553 made the set a
// construction local, so a test that drives a handler method directly has to
// receive it from the constructor rather than reach through the Server.
func newTestServerHS(p *mockPlatform) (*Server, *handlerSet) {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	return buildServerWithHandlers(ServerOptions{
		Addr:      ":0",
		Router:    router,
		Platforms: platforms,
		Backend:   "claude",
	})
}

// routerOf returns the concrete router a test fixture built srv with, for the
// test-only Router methods (InjectSession, Shutdown, …) serverRouter omits.
func routerOf(srv *Server) *session.Router {
	return srv.router.(*session.Router)
}

func newTestServer(p *mockPlatform) *Server {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	s := NewWithOptions(ServerOptions{
		Addr:      ":0",
		Router:    router,
		Platforms: platforms,
		Backend:   "claude",
	})
	return s
}

func newTestServerWithScheduler(p *mockPlatform) *Server {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	sched := cron.NewScheduler(cron.SchedulerConfig{}, cron.SchedulerDeps{})
	s := NewWithOptions(ServerOptions{
		Addr:      ":0",
		Router:    router,
		Platforms: platforms,
		Scheduler: sched,
		Backend:   "claude",
	})
	return s
}

func newTestServerWithToken(p *mockPlatform, token string) *Server {
	s, _ := newTestServerWithTokenHS(p, token)
	return s
}

// newTestServerWithTokenHS is newTestServerWithToken plus the handlerSet, for
// tests that drive a registration-only handler (the health probes) directly.
func newTestServerWithTokenHS(p *mockPlatform, token string) (*Server, *handlerSet) {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	return buildServerWithHandlers(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Platforms:      platforms,
		Backend:        "claude",
		DashboardToken: token,
	})
}

// newTestDispatcher returns the dispatcher the constructor built, so a test
// drives the production wiring rather than a copy of it.
func newTestDispatcher(srv *Server) *dispatch.Dispatcher {
	return srv.dispatcher
}

// ─── validateRemoteWorkspace (R61-SEC-2) ─────────────────────────────────────

func TestValidateRemoteWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty allowed (remote default)", "", false},
		{"absolute simple ok", "/home/user/proj", false},
		{"relative path rejected", "relative/path", true},
		{"traversal rejected", "/home/../etc", true},
		{"embedded traversal rejected", "/home/user/../../etc", true},
		{"control byte rejected", "/home/user/\nproj", true},
		{"tab rejected", "/home/user/\tproj", true},
		{"nul rejected", "/home/user/\x00proj", true},
		{"oversize rejected", "/" + strings.Repeat("a", 5000), true},
		{"DEL rejected", "/home/user/\x7fproj", true},
		// R176-SEC-M: C1 / bidi / LS / PS runes are >= 0x20 at the byte
		// level and slipped past the old ASCII-only loop. isLogInjectionRune
		// now rejects all four classes so they cannot reach slog attrs.
		{"C1 NEL rejected", "/home/user/\u0085proj", true},
		{"C1 DCS rejected", "/home/user/\u0090proj", true},
		{"bidi RLO rejected", "/home/user/\u202eproj", true},
		{"bidi LRI rejected", "/home/user/\u2066proj", true},
		{"line separator rejected", "/home/user/ proj", true},
		{"paragraph separator rejected", "/home/user/ proj", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRemoteWorkspace(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRemoteWorkspace(%q) err=%v wantErr=%v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// ─── handleHealth ─────────────────────────────────────────────────────────────

func TestHandleHealth_ReturnsJSONContentType(t *testing.T) {
	_, hs := newTestServerHS(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHandleHealth_StatusOkAndUptime(t *testing.T) {
	_, hs := newTestServerHS(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "ok" {
		t.Errorf("status = %v, want ok", result["status"])
	}
	uptime, ok := result["uptime"].(string)
	if !ok || uptime == "" {
		t.Errorf("uptime = %v, want non-empty string", result["uptime"])
	}
}

func TestHandleHealth_SessionsField(t *testing.T) {
	_, hs := newTestServerHS(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	hs.healthH.handleHealth(w, req)

	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sessions, ok := result["sessions"].(map[string]interface{})
	if !ok {
		t.Fatal("sessions field missing or wrong type")
	}
	if sessions["active"] != float64(0) {
		t.Errorf("active = %v, want 0", sessions["active"])
	}
	if sessions["total"] != float64(0) {
		t.Errorf("total = %v, want 0", sessions["total"])
	}
}

// ─── dedup filtering ─────────────────────────────────────────────────────────

func TestBuildMessageHandler_DedupEmptyEventIDFallbackFilters(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	// Per #1310: empty EventID falls back to a composite dedup key
	// (platform+chat+message+minute), so a same-minute retry is suppressed
	// even when the adapter left EventID empty.
	msg := platform.IncomingMessage{
		Platform: "test",
		EventID:  "",
		ChatID:   "chat1",
		Text:     "/cron",
	}
	handler(context.Background(), msg)
	before := p.replyCount()
	handler(context.Background(), msg)
	after := p.replyCount()

	if before == 0 {
		t.Error("first call should produce a reply")
	}
	if after != before {
		t.Errorf("retry with empty eventID must dedupe via fallback key: count went %d -> %d", before, after)
	}
}

// ─── /new command ────────────────────────────────────────────────────────────

func TestBuildMessageHandler_NewResetsGeneral(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServer(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "e1",
		ChatID:   "chat1",
		ChatType: "direct",
		Text:     "/new",
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if replies[0].Text != "对话已重置。" {
		t.Errorf("reply = %q, want %q", replies[0].Text, "对话已重置。")
	}
	if replies[0].ChatID != "chat1" {
		t.Errorf("reply chatID = %q, want chat1", replies[0].ChatID)
	}
}

func TestBuildMessageHandler_NewResetsNamedAgent(t *testing.T) {
	p := &mockPlatform{}
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	agentCommands := map[string]string{"review": "code-reviewer"}
	agents := map[string]session.AgentOpts{"code-reviewer": {}}
	srv := NewWithOptions(ServerOptions{
		Addr:      ":0",
		Router:    router,
		Platforms: platforms,
		Routing:   RoutingOptions{Agents: agents, AgentCommands: agentCommands},
		Backend:   "claude",
	})
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "e2",
		ChatID:   "chat1",
		Text:     "/new review",
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "code-reviewer") {
		t.Errorf("reply = %q, want to mention 'code-reviewer'", replies[0].Text)
	}
}

func TestBuildMessageHandler_NewUnknownAgentRepliesError(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServer(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "e3",
		ChatID:   "chat1",
		Text:     "/new bogus",
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "未知的 agent") {
		t.Errorf("reply = %q, want '未知的 agent' message", replies[0].Text)
	}
}

// ─── /cron dispatch ──────────────────────────────────────────────────────────

func TestBuildMessageHandler_CronNilSchedulerSilent(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServer(p) // scheduler is nil
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "c1",
		ChatID:   "chat1",
		Text:     "/cron",
	})

	if p.replyCount() != 0 {
		t.Errorf("nil scheduler: expected 0 replies, got %d", p.replyCount())
	}
}
