package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// ─── mock platform ────────────────────────────────────────────────────────────

type mockPlatform struct {
	mu      sync.Mutex
	replies []platform.OutgoingMessage
	maxLen  int
}

func (m *mockPlatform) Name() string                                               { return "test" }
func (m *mockPlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (m *mockPlatform) EditMessage(_ context.Context, _ string, _ string) error    { return nil }

func (m *mockPlatform) MaxReplyLength() int {
	if m.maxLen <= 0 {
		return 4000
	}
	return m.maxLen
}

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

func newTestServer(p *mockPlatform) *Server {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	s := New(":0", router, platforms, nil, nil, nil, "claude", ServerOptions{})
	s.registerDashboard()
	return s
}

func newTestServerWithScheduler(p *mockPlatform) *Server {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	sched := cron.NewScheduler(cron.SchedulerConfig{})
	s := New(":0", router, platforms, nil, nil, sched, "claude", ServerOptions{})
	s.registerDashboard()
	return s
}

func newTestServerWithToken(p *mockPlatform, token string) *Server {
	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": p}
	s := New(":0", router, platforms, nil, nil, nil, "claude", ServerOptions{DashboardToken: token})
	s.registerDashboard()
	return s
}

func newTestDispatcher(srv *Server) *dispatch.Dispatcher {
	return dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:        srv.router,
		Platforms:     srv.platforms,
		Agents:        srv.agents,
		AgentCommands: srv.agentCommands,
		Scheduler:     srv.scheduler,
		ProjectMgr:    srv.projectMgr,
		Guard:         srv.sessionGuard,
		Dedup:         srv.dedup,
		AllowedRoot:   srv.allowedRoot,
		ClaudeDir:     srv.claudeDir,
		ReplyFooter:   srv.backendTag,
		SendFn: func(ctx context.Context, key string, sess *session.ManagedSession, text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error) {
			return sess.Send(ctx, text, images, onEvent)
		},
		TakeoverFn: func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
			return false
		},
		NoOutputTimeout:       srv.noOutputTimeout,
		TotalTimeout:          srv.totalTimeout,
		WatchdogNoOutputKills: &srv.watchdogNoOutputKills,
		WatchdogTotalKills:    &srv.watchdogTotalKills,
	})
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRemoteWorkspace(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRemoteWorkspace(%q) err=%v wantErr=%v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// ─── handleHealth ─────────────────────────────────────────────────────────────

func TestHandleHealth_ReturnsJSONContentType(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHandleHealth_StatusOkAndUptime(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

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
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

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

func TestBuildMessageHandler_DedupDuplicateEventID(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	msg := platform.IncomingMessage{
		Platform: "test",
		EventID:  "evt-001",
		ChatID:   "chat1",
		Text:     "/cron",
	}
	handler(context.Background(), msg)
	after1 := p.replyCount()

	handler(context.Background(), msg) // duplicate eventID
	after2 := p.replyCount()

	if after1 == 0 {
		t.Error("first call should produce a reply")
	}
	if after2 != after1 {
		t.Errorf("duplicate eventID must not produce another reply: count went %d -> %d", after1, after2)
	}
}

func TestBuildMessageHandler_DedupEmptyEventIDNotFiltered(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	// empty EventID is never recorded by dedup
	msg := platform.IncomingMessage{
		Platform: "test",
		EventID:  "",
		ChatID:   "chat1",
		Text:     "/cron",
	}
	handler(context.Background(), msg)
	handler(context.Background(), msg)

	if p.replyCount() < 2 {
		t.Errorf("empty eventID should not deduplicate: got %d replies, want >= 2", p.replyCount())
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
	srv := New(":0", router, platforms, agents, agentCommands, nil, "claude", ServerOptions{})
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

func TestBuildMessageHandler_CronNoSubcommandShowsUsage(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "c2",
		ChatID:   "chat1",
		Text:     "/cron",
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply for /cron usage, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "/cron add") {
		t.Errorf("expected usage text containing '/cron add', got %q", replies[0].Text)
	}
}

func TestBuildMessageHandler_CronListEmpty(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "c3",
		ChatID:   "chat1",
		Text:     "/cron list",
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply for /cron list, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "没有定时任务") {
		t.Errorf("expected empty-list message, got %q", replies[0].Text)
	}
}

func TestBuildMessageHandler_CronAddInvalidSchedule(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "c4",
		ChatID:   "chat1",
		Text:     `/cron add "not-a-valid-cron" some prompt`,
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "创建失败") {
		t.Errorf("expected '创建失败' message, got %q", replies[0].Text)
	}
}

func TestBuildMessageHandler_CronAddMissingPrompt(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "c5",
		ChatID:   "chat1",
		Text:     `/cron add "@every 1h"`,
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "格式错误") {
		t.Errorf("expected '格式错误' message, got %q", replies[0].Text)
	}
}

func TestBuildMessageHandler_CronDelMissingID(t *testing.T) {
	p := &mockPlatform{}
	srv := newTestServerWithScheduler(p)
	handler := newTestDispatcher(srv).BuildHandler()

	handler(context.Background(), platform.IncomingMessage{
		Platform: "test",
		EventID:  "c6",
		ChatID:   "chat1",
		Text:     "/cron del",
	})

	replies := p.allReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "用法") {
		t.Errorf("expected usage message, got %q", replies[0].Text)
	}
}

// ─── sendSplitReply ──────────────────────────────────────────────────────────

func TestSendSplitReply_ShortMessageSingleReply(t *testing.T) {
	p := &mockPlatform{maxLen: 100}
	srv := newTestServer(p)

	newTestDispatcher(srv).SendSplitReply(context.Background(), p, "chat1", "hello world")

	if p.replyCount() != 1 {
		t.Fatalf("expected 1 reply, got %d", p.replyCount())
	}
	if p.allReplies()[0].Text != "hello world" {
		t.Errorf("reply text = %q, want %q", p.allReplies()[0].Text, "hello world")
	}
}

func TestSendSplitReply_LongMessageSplitsCorrectly(t *testing.T) {
	p := &mockPlatform{maxLen: 10}
	srv := newTestServer(p)

	text := strings.Repeat("a", 35)
	newTestDispatcher(srv).SendSplitReply(context.Background(), p, "chat1", text)

	if p.replyCount() < 2 {
		t.Fatalf("expected >= 2 replies for 35-char text with maxLen=10, got %d", p.replyCount())
	}
	combined := ""
	for _, r := range p.allReplies() {
		// Strip the trailing split marker (e.g., "\n— [1/4]") before checking
		chunk := r.Text
		if idx := strings.LastIndex(chunk, "\n— ["); idx >= 0 {
			chunk = chunk[:idx]
		}
		if len(chunk) > 10 {
			t.Errorf("reply content len %d exceeds maxLen 10: %q", len(chunk), chunk)
		}
		combined += chunk
	}
	if combined != text {
		t.Errorf("combined replies = %q, want %q", combined, text)
	}
}

func TestSendSplitReply_ZeroMaxLenDefaultsTo4000(t *testing.T) {
	p := &mockPlatform{maxLen: 0} // triggers default 4000
	srv := newTestServer(p)

	newTestDispatcher(srv).SendSplitReply(context.Background(), p, "chat1", "short")

	if p.replyCount() != 1 {
		t.Errorf("expected 1 reply with 4000 default maxLen, got %d", p.replyCount())
	}
}

func TestSendSplitReply_ExactlyMaxLen(t *testing.T) {
	p := &mockPlatform{maxLen: 5}
	srv := newTestServer(p)

	newTestDispatcher(srv).SendSplitReply(context.Background(), p, "chat1", "hello")

	if p.replyCount() != 1 {
		t.Errorf("expected 1 reply for text exactly at maxLen, got %d", p.replyCount())
	}
}

func TestSendSplitReply_ChatIDForwarded(t *testing.T) {
	p := &mockPlatform{maxLen: 100}
	srv := newTestServer(p)

	newTestDispatcher(srv).SendSplitReply(context.Background(), p, "room-xyz", "some text")

	for _, r := range p.allReplies() {
		if r.ChatID != "room-xyz" {
			t.Errorf("reply ChatID = %q, want room-xyz", r.ChatID)
		}
	}
}

// ─── splitText edge cases ────────────────────────────────────────────────────

func TestSplitText(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxLen int
		want   int // expected number of chunks
	}{
		{"short text", "hello", 100, 1},
		{"exact limit", "abcde", 5, 1},
		{"needs split", "abcdefgh", 4, 2},
		{"empty text", "", 100, 1},
		{"split at newline", "abc\ndef\nghi", 6, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := platform.SplitText(tt.text, tt.maxLen)
			if len(chunks) != tt.want {
				t.Errorf("platform.SplitText(%q, %d) = %d chunks, want %d: %v",
					tt.text, tt.maxLen, len(chunks), tt.want, chunks)
			}
			joined := ""
			for _, c := range chunks {
				joined += c
			}
			if joined != tt.text {
				t.Errorf("joined chunks = %q, want %q", joined, tt.text)
			}
		})
	}
}

func TestSplitTextLong(t *testing.T) {
	// 10 chars, maxLen=3 -> 4 chunks
	chunks := platform.SplitText("0123456789", 3)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4: %v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if len(c) > 3 {
			t.Errorf("chunk[%d] len=%d, max 3: %q", i, len(c), c)
		}
	}
}

func TestSplitTextPreferNewline(t *testing.T) {
	// "aaa\nbbb" with maxLen=5 should split at the newline
	text := "aaa\nbbb"
	chunks := platform.SplitText(text, 5)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %v", len(chunks), chunks)
	}
	if chunks[0] != "aaa\n" {
		t.Errorf("chunk[0] = %q, want %q", chunks[0], "aaa\n")
	}
	if chunks[1] != "bbb" {
		t.Errorf("chunk[1] = %q, want %q", chunks[1], "bbb")
	}
}

func TestSplitTextNewlineInSecondHalf(t *testing.T) {
	// "hello\nworld" (11 chars), maxLen=8 — newline at idx 5 > maxLen/2=4
	// should split at the newline rather than at index 8
	text := "hello\nworld"
	chunks := platform.SplitText(text, 8)
	joined := strings.Join(chunks, "")
	if joined != text {
		t.Errorf("joined = %q, want %q", joined, text)
	}
	if !strings.HasSuffix(chunks[0], "\n") {
		t.Errorf("first chunk should end at newline, got %q", chunks[0])
	}
}

func TestSplitTextMultipleLines(t *testing.T) {
	text := "line1\nline2\nline3\nline4"
	chunks := platform.SplitText(text, 12)
	joined := strings.Join(chunks, "")
	if joined != text {
		t.Errorf("joined = %q, want %q", joined, text)
	}
	for i, c := range chunks {
		if len(c) > 12 {
			t.Errorf("chunk[%d] len %d exceeds 12: %q", i, len(c), c)
		}
	}
}

func TestSplitTextNoNewline(t *testing.T) {
	// No newlines — must split on byte boundary
	text := "abcdefghijklmno" // 15 chars
	chunks := platform.SplitText(text, 5)
	joined := strings.Join(chunks, "")
	if joined != text {
		t.Errorf("joined = %q, want %q", joined, text)
	}
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d: %v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if len(c) > 5 {
			t.Errorf("chunk[%d] len %d exceeds 5: %q", i, len(c), c)
		}
	}
}

func TestSplitTextSingleChar(t *testing.T) {
	chunks := platform.SplitText("x", 1)
	if len(chunks) != 1 || chunks[0] != "x" {
		t.Errorf("single char: got %v, want [x]", chunks)
	}
}

// ─── parseCronAdd ────────────────────────────────────────────────────────────

func TestParseCronAdd(t *testing.T) {
	tests := []struct {
		args         string
		wantSchedule string
		wantPrompt   string
		wantErr      bool
	}{
		{`"@every 30m" check status`, "@every 30m", "check status", false},
		{`"0 9 * * 1-5" /review scan PRs`, "0 9 * * 1-5", "/review scan PRs", false},
		{`"@daily" summarize`, "@daily", "summarize", false},
		{`"@every 1h"`, "", "", true},    // missing prompt
		{`no quotes here`, "", "", true}, // no opening quote
		{`"unclosed`, "", "", true},      // no closing quote
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			schedule, prompt, err := dispatch.ParseCronAdd(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCronAdd(%q): err=%v, wantErr=%v", tt.args, err, tt.wantErr)
				return
			}
			if schedule != tt.wantSchedule {
				t.Errorf("schedule = %q, want %q", schedule, tt.wantSchedule)
			}
			if prompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", prompt, tt.wantPrompt)
			}
		})
	}
}

// ─── routing ────────────────────────────────────────────────────────────────

func TestResolveAgent(t *testing.T) {
	cmds := map[string]string{
		"review":   "code-reviewer",
		"research": "researcher",
	}

	tests := []struct {
		text      string
		wantAgent string
		wantText  string
	}{
		{"hello world", "general", "hello world"},
		{"/review PR#123", "code-reviewer", "PR#123"},
		{"/research quantum computing", "researcher", "quantum computing"},
		{"/review", "code-reviewer", ""},
		{"/unknown cmd", "general", "/unknown cmd"},
		{"/new", "general", "/new"},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			agent, text := session.ResolveAgent(tt.text, cmds)
			if agent != tt.wantAgent {
				t.Errorf("ResolveAgent(%q).agent = %q, want %q", tt.text, agent, tt.wantAgent)
			}
			if text != tt.wantText {
				t.Errorf("ResolveAgent(%q).text = %q, want %q", tt.text, text, tt.wantText)
			}
		})
	}
}
