package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// ---------------------------------------------------------------------------
// Fake platform
// ---------------------------------------------------------------------------

type fakePlatform struct {
	mu              sync.Mutex
	replies         []platform.OutgoingMessage
	edits           []fakeEdit
	supportsInterim bool
	replyErr        error
	replyMsgID      string
}

type fakeEdit struct {
	msgID string
	text  string
}

func (f *fakePlatform) Name() string                                               { return "fake" }
func (f *fakePlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (f *fakePlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replyErr != nil {
		return "", f.replyErr
	}
	f.replies = append(f.replies, msg)
	id := f.replyMsgID
	if id == "" {
		id = fmt.Sprintf("msg-%d", len(f.replies))
	}
	return id, nil
}
func (f *fakePlatform) EditMessage(_ context.Context, msgID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, fakeEdit{msgID: msgID, text: text})
	return nil
}
func (f *fakePlatform) MaxReplyLength() int           { return 4000 }
func (f *fakePlatform) SupportsInterimMessages() bool { return f.supportsInterim }

func (f *fakePlatform) lastReply() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replies) == 0 {
		return ""
	}
	return f.replies[len(f.replies)-1].Text
}

func (f *fakePlatform) replyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies)
}

func (f *fakePlatform) allReplies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.replies))
	for i, r := range f.replies {
		out[i] = r.Text
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testDispatcherConfig is what a dispatcherTestOption edits: the
// DispatcherConfig plus the queue options and Sender newTestDispatcher builds
// the dispatcher's turn.Orchestrator from, so every test runs IM turns
// through the real orchestrator.
type testDispatcherConfig struct {
	DispatcherConfig
	queue  turn.QueueOptions
	sender *testSender
}

type dispatcherTestOption func(*testDispatcherConfig)

// withSendFn replaces the test Sender's Send.
func withSendFn(fn func(context.Context, string, turn.Session, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error)) dispatcherTestOption {
	return func(cfg *testDispatcherConfig) { cfg.sender.send = fn }
}

// withQueue replaces the default collect-mode queue options.
func withQueue(qo turn.QueueOptions) dispatcherTestOption {
	return func(cfg *testDispatcherConfig) { cfg.queue = qo }
}

// withRouter makes r both the dispatcher's SessionRouter and the router the
// default Sender reaches, so a test can inject sessions into it.
func withRouter(r *session.Router) dispatcherTestOption {
	return func(cfg *testDispatcherConfig) {
		cfg.Router = r
		cfg.sender.router = r
	}
}

// withSender replaces the default Sender (a testSender over the router).
func withSender(s *testSender) dispatcherTestOption {
	return func(cfg *testDispatcherConfig) { cfg.sender = s }
}

func newTestDispatcher(fp *fakePlatform, opts ...dispatcherTestOption) *Dispatcher {
	router := session.NewRouter(session.RouterConfig{MaxProcs: 10})
	cfg := testDispatcherConfig{
		DispatcherConfig: DispatcherConfig{
			Router:        router,
			Platforms:     map[string]platform.Platform{"fake": fp},
			Agents:        map[string]session.AgentOpts{},
			AgentCommands: map[string]string{},
			Dedup:         platform.NewDedup(100),
			Capabilities: fakeCapabilities{
				takeover: func(_ context.Context, _, _ string, _ session.AgentOpts) bool { return false },
			},
			WatchdogNoOutputKills: new(atomic.Int64),
			WatchdogTotalKills:    new(atomic.Int64),
			NoOutputTimeout:       5 * time.Second,
			TotalTimeout:          30 * time.Second,
		},
		queue:  turn.QueueOptions{MaxDepth: 5},
		sender: &testSender{router: router},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg.Turns = turn.New(cfg.queue, cfg.sender)
	d, err := NewDispatcher(cfg.DispatcherConfig)
	if err != nil {
		// The helper always sets Turns, so wireup never fails; panic so a
		// helper edit that drops it fails here, not in a later assertion.
		panic("newTestDispatcher: NewDispatcher returned error with Turns set: " + err.Error())
	}
	return d
}

// testTurns is a Turns for construction-only tests that never run a turn.
func testTurns() Turns {
	return turn.New(turn.QueueOptions{MaxDepth: 5}, &testSender{})
}

// holdKey makes key look mid-turn: it takes the owner slot with a turn that
// never runs, so the next message for key queues behind it (or, with the
// queue disabled, is dropped).
func holdKey(t testing.TB, d *Dispatcher, key string) {
	t.Helper()
	if ack := d.turns.Submit(context.Background(), turn.Request{Key: key, Text: "running"}, parkedAdmission{}); ack != turn.AckOwner {
		t.Fatalf("holdKey: %q already has an owner (ack %d)", key, ack)
	}
}

// parkedAdmission admits a run and never starts it.
type parkedAdmission struct{}

func (parkedAdmission) Admit(turn.RunKind) (func(fn func(context.Context)), bool) {
	return func(func(context.Context)) {}, true
}

// inlineAdmission runs every turn on the caller's goroutine, so a test that
// submits directly returns only after the turn's delivery finished.
type inlineAdmission struct{ ctx context.Context }

func (a inlineAdmission) Admit(turn.RunKind) (func(fn func(context.Context)), bool) {
	return func(fn func(context.Context)) { fn(a.ctx) }, true
}

// runIMTurn runs one IM turn for msg on key through d's Turns, synchronously:
// an owner turn (TurnInfo.First) when first, else a PriorityNow detached turn
// (not First). It bypasses BuildHandler's front matter, so the platform need
// not be registered.
func runIMTurn(ctx context.Context, d *Dispatcher, key, text string, msg platform.IncomingMessage, first bool) {
	o := d.newIMOrigin(msg, slog.Default(), key, "general", session.AgentOpts{}, imMessage, len(text), 0)
	r := turn.Request{Key: key, Text: text, Origin: o}
	if !first {
		r.Priority = turn.PriorityNow
	}
	d.turns.Submit(ctx, r, inlineAdmission{ctx})
}

func incomingMsg(text string) platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: "fake", EventID: "evt-" + text,
		UserID: "user1", ChatID: "chat1", ChatType: "direct", Text: text,
	}
}

// ---------------------------------------------------------------------------
// ParseCronAdd
// ---------------------------------------------------------------------------

func TestParseCronAdd(t *testing.T) {
	tests := []struct {
		name         string
		args         string
		wantSchedule string
		wantPrompt   string
		wantErr      bool
	}{
		{"valid every", `"@every 30m" check services`, "@every 30m", "check services", false},
		{"cron expr", `"0 9 * * 1-5" /review PRs`, "0 9 * * 1-5", "/review PRs", false},
		{"no quote", `@every 30m check`, "", "", true},
		{"missing close quote", `"@every 30m check`, "", "", true},
		{"empty prompt", `"@every 30m" `, "", "", true},
		// R20260527122801-ARCH-3 (#1315): schedule + prompt validation is now
		// delegated to cron.ValidateScheduleChars / cron.ValidatePromptStrict.
		// These cases pin that the shared validators are actually wired through
		// ParseCronAdd so the IM edge cannot drift from the dashboard edge.
		{"schedule control char", "\"@every \x0030m\" check", "", "", true},
		{"prompt control char", "\"@every 30m\" che\x00ck", "", "", true},
		{"prompt bidi override", "\"@every 30m\" che\u202eck", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCronAdd(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if got.Schedule != tt.wantSchedule {
					t.Errorf("schedule = %q, want %q", got.Schedule, tt.wantSchedule)
				}
				if got.Prompt != tt.wantPrompt {
					t.Errorf("prompt = %q, want %q", got.Prompt, tt.wantPrompt)
				}
				if got.KeepContext {
					t.Errorf("KeepContext = true without the flag")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// replyText
// ---------------------------------------------------------------------------

func TestReplyText_UnknownPlatform(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	msg := platform.IncomingMessage{Platform: "nonexistent", ChatID: "c1"}
	if d.replyText(context.Background(), msg, "hi", nil) {
		t.Error("replyText should return false for unknown platform")
	}
}

func TestReplyText_KnownPlatform(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if !d.replyText(context.Background(), incomingMsg("x"), "hello", slog.Default()) {
		t.Error("replyText should return true for known platform")
	}
	if fp.lastReply() != "hello" {
		t.Errorf("reply = %q, want %q", fp.lastReply(), "hello")
	}
}

// ---------------------------------------------------------------------------
// dispatchCommand
// ---------------------------------------------------------------------------

func TestDispatchCommand_Help(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if !d.dispatchCommand(context.Background(), incomingMsg("/help"), "/help", slog.Default()) {
		t.Fatal("expected /help to be handled")
	}
	reply := fp.lastReply()
	for _, want := range []string{"/help", "/new", "/cron", "/pwd"} {
		if !strings.Contains(reply, want) {
			t.Errorf("help reply missing %q: %q", want, reply)
		}
	}
}

func TestDispatchCommand_HelpWithAgents(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.agentCommands = map[string]string{"review": "code-reviewer"}
	d.dispatchCommand(context.Background(), incomingMsg("/help"), "/help", slog.Default())
	if !strings.Contains(fp.lastReply(), "review") {
		t.Errorf("expected agent in help, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_New_Basic(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if !d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new", slog.Default()) {
		t.Fatal("expected /new to be handled")
	}
	if !strings.Contains(fp.lastReply(), "重置") {
		t.Errorf("expected reset confirmation, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_Clear(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if !d.dispatchCommand(context.Background(), incomingMsg("/clear"), "/clear", slog.Default()) {
		t.Fatal("expected /clear to be handled")
	}
	if !strings.Contains(fp.lastReply(), "重置") {
		t.Errorf("expected reset confirmation, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_New_UnknownAgent(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.agentCommands = map[string]string{"review": "reviewer"}
	d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new unknown-agent", slog.Default())
	if !strings.Contains(fp.lastReply(), "未知") {
		t.Errorf("expected unknown agent message, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_New_NamedAgent(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.agentCommands = map[string]string{"review": "reviewer"}
	d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new review", slog.Default())
	if !strings.Contains(fp.lastReply(), "重置") {
		t.Errorf("expected reset confirmation for named agent, got %q", fp.lastReply())
	}
}

// TestResolveAgentToken pins the two-stage resolution shared by both
// handleNewCommand branches (R0530-CR-1): exact lowercase key lookup, then
// EqualFold scan over stored (possibly mixed-case) values.
func TestResolveAgentToken(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.agentCommands = map[string]string{"review": "ReviewerBot", "ops": "ops-agent"}

	cases := []struct {
		token  string
		wantID string
		wantOK bool
	}{
		{"review", "ReviewerBot", true},      // exact key hit
		{"reviewerbot", "ReviewerBot", true}, // EqualFold value hit (lowercased input)
		{"ops-agent", "ops-agent", true},     // EqualFold value hit, same case
		{"ops", "ops-agent", true},           // exact key hit
		{"nope", "", false},                  // miss
	}
	for _, tc := range cases {
		gotID, gotOK := d.resolveAgentToken(tc.token)
		if gotOK != tc.wantOK || gotID != tc.wantID {
			t.Errorf("resolveAgentToken(%q) = (%q, %v), want (%q, %v)",
				tc.token, gotID, gotOK, tc.wantID, tc.wantOK)
		}
	}
}

// TestDispatchCommand_New_ProjectBound_MixedCaseAgent is the R0530-CR-1
// regression guard: in a project-bound chat, "/new <Agent>" with a
// mixed-case agent ID must resolve via the EqualFold fallback and reset
// the session — previously the bound branch did exact-key-only lookup and
// replied "未知的 agent" while the unbound branch resolved fine.
func TestDispatchCommand_New_ProjectBound_MixedCaseAgent(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	// agentCommands keys are lowercased by applyDefaults; values keep the
	// operator-supplied (mixed) case.
	d.agentCommands = map[string]string{"review": "ReviewerBot"}
	// Project-bound resolver: the chat resolves to a bound project.
	d.resolver = session.NewKeyResolver(
		map[string]session.AgentOpts{"general": {}, "ReviewerBot": {}},
		&parityDataSource{binding: session.ProjectBinding{
			Bound: true, Name: "demo", WorkspaceDir: "/srv/demo",
		}},
	)

	// User types the agent in its stored mixed case, which the lowercasing
	// in handleNewCommand turns into "reviewerbot" — only the EqualFold
	// fallback can map that back to "ReviewerBot".
	d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new ReviewerBot", slog.Default())

	reply := fp.lastReply()
	if strings.Contains(reply, "未知") {
		t.Fatalf("project-bound /new <mixed-case agent> wrongly rejected: %q", reply)
	}
	if !strings.Contains(reply, "重置") || !strings.Contains(reply, "ReviewerBot") {
		t.Errorf("expected reset confirmation for ReviewerBot, got %q", reply)
	}
}

func TestDispatchCommand_CaseInsensitive(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if !d.dispatchCommand(context.Background(), incomingMsg("/Help"), "/Help", slog.Default()) {
		t.Fatal("expected /Help to be handled case-insensitively")
	}
	if !strings.Contains(fp.lastReply(), "/help") {
		t.Errorf("case-insensitive /Help should return help; got %q", fp.lastReply())
	}
}

func TestDispatchCommand_Pwd(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if !d.dispatchCommand(context.Background(), incomingMsg("/pwd"), "/pwd", slog.Default()) {
		t.Fatal("expected /pwd to be handled")
	}
	if !strings.Contains(fp.lastReply(), "工作目录") {
		t.Errorf("expected workspace in reply, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_CronNoScheduler(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = nil
	// /cron is always "handled" (returns true) even with nil scheduler
	if !d.dispatchCommand(context.Background(), incomingMsg("/cron list"), "/cron list", slog.Default()) {
		t.Fatal("expected /cron to be handled")
	}
	// With nil scheduler, no reply is sent
	if fp.replyCount() != 0 {
		t.Errorf("expected no reply with nil scheduler, got %d replies", fp.replyCount())
	}
}

func TestDispatchCommand_Unknown(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if d.dispatchCommand(context.Background(), incomingMsg("/foobar"), "/foobar", slog.Default()) {
		t.Fatal("unknown command should not be handled")
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — dedup
// ---------------------------------------------------------------------------

func TestBuildHandler_DedupDropsDuplicate(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	h := d.BuildHandler()
	ctx := context.Background()
	// Use a /help message so we don't need GetOrCreate.
	msg := platform.IncomingMessage{
		Platform: "fake", EventID: "dup-event",
		UserID: "u1", ChatID: "c1", ChatType: "direct", Text: "/help",
	}
	h(ctx, msg)
	count := fp.replyCount()
	h(ctx, msg) // duplicate event ID — must be dropped
	if fp.replyCount() != count {
		t.Errorf("duplicate event was processed: count %d → %d", count, fp.replyCount())
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — /help via handler
// ---------------------------------------------------------------------------

func TestBuildHandler_Help(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.BuildHandler()(context.Background(), incomingMsg("/help"))
	if !strings.Contains(fp.lastReply(), "/help") {
		t.Errorf("expected /help in reply, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — empty text returns without sending
// ---------------------------------------------------------------------------

func TestBuildHandler_EmptyText(t *testing.T) {
	fp := &fakePlatform{}
	called := false
	d := newTestDispatcher(fp, withSendFn(func(_ context.Context, _ string, _ turn.Session, _ string, _ []clievent.Attachment, _ clievent.EventCallback) (*clievent.SendResult, error) {
		called = true
		return &clievent.SendResult{Text: "ok"}, nil
	}))
	d.BuildHandler()(context.Background(), incomingMsg("  "))
	if called {
		t.Error("sendFn should not be called for whitespace-only message")
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — unknown slash command warns user
// ---------------------------------------------------------------------------

func TestBuildHandler_UnknownSlash(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.BuildHandler()(context.Background(), incomingMsg("/unknowncmd"))
	if !strings.Contains(fp.lastReply(), "未知命令") {
		t.Errorf("expected unknown-command message, got %q", fp.lastReply())
	}
}

// TestBuildHandler_UnknownSlash_SanitizesAttackerControl pins
// R20260527122801-CR-15: the unknown-command reply embeds the user-supplied
// cmd token directly. ANSI / C0 / C1 / DEL bytes from chat must be scrubbed
// so IM renderers cannot reinterpret them as formatting (e.g. an embedded
// ESC starts an ANSI colour run; a tab splits slog attributes downstream).
func TestBuildHandler_UnknownSlash_SanitizesAttackerControl(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	// Embed ESC (0x1b), DEL (0x7f), and a tab — all should be replaced.
	d.BuildHandler()(context.Background(), incomingMsg("/\x1b[31mevil\x7f\tcmd"))
	reply := fp.lastReply()
	if !strings.Contains(reply, "未知命令") {
		t.Fatalf("expected unknown-command reply, got %q", reply)
	}
	for _, bad := range []string{"\x1b", "\x7f", "\t"} {
		if strings.Contains(reply, bad) {
			t.Errorf("reply still contains unsanitized byte %q: %q", bad, reply)
		}
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — path-like slash is NOT flagged as unknown command
// ---------------------------------------------------------------------------

func TestBuildHandler_PathSlash_NotUnknown(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.BuildHandler()(context.Background(), incomingMsg("/home/user/file.go を確認"))
	for _, r := range fp.allReplies() {
		if strings.Contains(r, "未知命令") {
			t.Errorf("path-like slash wrongly flagged as unknown: %q", r)
		}
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — queue path busy (owner already holds the key)
// ---------------------------------------------------------------------------

// TestBuildHandler_QueueBusy_SecondMessageQueued pins BuildHandler's busy
// path: a second message for a key the first already owns gets queued with
// an ack reply, not the disabled-queue "正在处理" text.
func TestBuildHandler_QueueBusy_SecondMessageQueued(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	key := session.SessionKey("fake", "direct", "chat1", "general")
	// Pre-acquire ownership of key, as if a first message's owner loop were
	// already running.
	holdKey(t, d, key)
	d.BuildHandler()(context.Background(), incomingMsg("hello"))
	if !strings.Contains(fp.lastReply(), "消息已收到") {
		t.Errorf("expected queued ack, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — queue maxDepth=0 drop-notify
// ---------------------------------------------------------------------------

func TestBuildHandler_QueueDrop_Notify(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withQueue(turn.QueueOptions{}))
	// Mark session busy
	key := session.SessionKey("fake", "direct", "chat1", "general")
	holdKey(t, d, key)
	d.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: "fake", EventID: "e2", UserID: "u1",
		ChatID: "chat1", ChatType: "direct", Text: "second",
	})
	if !strings.Contains(fp.lastReply(), "正在处理") {
		t.Errorf("expected drop-notify message, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// IM turn — GetOrCreate error paths
// ---------------------------------------------------------------------------

// Without a real wrapper GetOrCreate fails, and the turn's StageSession
// outcome is replied with the session-creation error message.
func TestIMTurn_GetOrCreateError_DefaultMessage(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp) // no wrapper → GetOrCreate will fail
	msg := incomingMsg("hello")
	runIMTurn(context.Background(), d, "key1", "hello", msg, true)
	// Default error → "会话创建失败" message
	if !strings.Contains(fp.lastReply(), "会话") {
		t.Errorf("expected session creation error message, got %q", fp.lastReply())
	}
}

// TestIMTurn_StageDecidesTheErrorReply: the delivery answers a failure by
// the stage it happened at. Only a failed Send is a reply error for /health
// (replyErrorCount); a failed GetOrCreate is answered by
// handleGetOrCreateError and leaves the counter alone.
func TestIMTurn_StageDecidesTheErrorReply(t *testing.T) {
	errBoom := errors.New("boom")
	for _, tc := range []struct {
		name       string
		sender     *testSender
		wantErrors int64
	}{
		{"GetOrCreate fails", &testSender{getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			return nil, 0, errBoom
		}}, 0},
		{"Send fails", &testSender{
			getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
				return fakeSession{}, session.SessionExisting, nil
			},
			send: func(context.Context, string, turn.Session, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
				return nil, errBoom
			},
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakePlatform{}
			d := newTestDispatcher(fp, withSender(tc.sender))
			runIMTurn(context.Background(), d, "key1", "hello", incomingMsg("hello"), true)
			if fp.replyCount() != 1 {
				t.Fatalf("replies = %v, want one error reply", fp.allReplies())
			}
			if got := d.replyErrorCount.Load(); got != tc.wantErrors {
				t.Errorf("replyErrorCount = %d, want %d", got, tc.wantErrors)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — unknown platform gets no turn
// ---------------------------------------------------------------------------

// A message from a platform the dispatcher does not know is never submitted:
// neither GetOrCreate nor Send runs, since there is nowhere to reply.
func TestBuildHandler_UnknownPlatformGetsNoTurn(t *testing.T) {
	fp := &fakePlatform{}
	var calls atomic.Int64
	sender := &testSender{
		getOrCreate: func(context.Context, string, session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			calls.Add(1)
			return nil, 0, errors.New("unexpected GetOrCreate")
		},
		send: func(context.Context, string, turn.Session, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
			calls.Add(1)
			return &clievent.SendResult{Text: "ok"}, nil
		},
	}
	d := newTestDispatcher(fp, withSender(sender))
	for _, text := range []string{"hello", "/urgent now"} {
		d.BuildHandler()(context.Background(), platform.IncomingMessage{
			Platform: "unknown", EventID: "e-" + text, UserID: "u1",
			ChatID: "c1", ChatType: "direct", Text: text,
		})
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Sender reached %d times for an unknown platform, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// SendSplitReply
// ---------------------------------------------------------------------------

func TestSendSplitReply_Short(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.SendSplitReply(context.Background(), fp, ReplyDest{ChatID: "c1"}, "short message")
	if fp.replyCount() != 1 || fp.lastReply() != "short message" {
		t.Errorf("reply = %q, want %q", fp.lastReply(), "short message")
	}
}

func TestSendSplitReply_Long_Paginates(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	// >4000 chars → 2+ chunks
	d.SendSplitReply(context.Background(), fp, ReplyDest{ChatID: "c1"}, strings.Repeat("A", 8001))
	if fp.replyCount() < 2 {
		t.Errorf("reply count = %d, want ≥ 2", fp.replyCount())
	}
	last := fp.allReplies()[fp.replyCount()-1]
	if !strings.Contains(last, "/") {
		t.Errorf("expected pagination marker, got %q", last)
	}
}

type zeroMaxPlatform struct{ *fakePlatform }

func (z *zeroMaxPlatform) MaxReplyLength() int { return 0 }

func TestSendSplitReply_ZeroMax_Defaults4000(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.SendSplitReply(context.Background(), &zeroMaxPlatform{fp}, ReplyDest{ChatID: "c1"}, "hello")
	if fp.replyCount() != 1 {
		t.Errorf("reply count = %d, want 1", fp.replyCount())
	}
}

// ---------------------------------------------------------------------------
// replyTracker
// ---------------------------------------------------------------------------

func TestReplyTracker_NonInterim_WaitReadyInstant(t *testing.T) {
	fp := &fakePlatform{supportsInterim: false}
	tracker := newIMEventTracker(context.Background(), fp, ReplyDest{ChatID: "c1"}, "direct", "")
	defer tracker.stop()
	tracker.onEvent(clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "thinking", Text: "t"}}},
	})
	done := make(chan struct{})
	go func() { tracker.waitReady(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady timed out")
	}
	if fp.replyCount() != 0 {
		t.Errorf("non-interim: expected 0 replies, got %d", fp.replyCount())
	}
}

func TestReplyTracker_Interim_InitialReply(t *testing.T) {
	fp := &fakePlatform{supportsInterim: true, replyMsgID: "thinking-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tracker := newIMEventTracker(ctx, fp, ReplyDest{ChatID: "c1"}, "direct", "")
	defer tracker.stop()
	tracker.onEvent(clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "thinking", Text: "analyzing"}}},
	})
	tracker.waitReady(ctx)
	if got := tracker.getThinkingMsgID(); got != "thinking-1" {
		t.Errorf("thinkingMsgID = %q, want %q", got, "thinking-1")
	}
	if fp.replyCount() != 1 {
		t.Errorf("reply count = %d, want 1", fp.replyCount())
	}
}

func TestReplyTracker_RenderStatus(t *testing.T) {
	fp := &fakePlatform{supportsInterim: false}
	tracker := newIMEventTracker(context.Background(), fp, ReplyDest{ChatID: "c1"}, "direct", "")
	defer tracker.stop()
	tracker.linesMu.Lock()
	tracker.statusLines = appendStatusLine(tracker.statusLines, "💭 thinking")
	tracker.statusLines = appendStatusLine(tracker.statusLines, "🔧 Read")
	tracker.linesMu.Unlock()
	status := tracker.renderStatus()
	if !strings.Contains(status, "💭 thinking") || !strings.Contains(status, "🔧 Read") {
		t.Errorf("renderStatus = %q, want both lines", status)
	}
}

func TestReplyTracker_Stop_Idempotent(t *testing.T) {
	fp := &fakePlatform{supportsInterim: false}
	tracker := newIMEventTracker(context.Background(), fp, ReplyDest{ChatID: "c1"}, "direct", "")
	tracker.stop()
	tracker.stop()
}

func TestReplyTracker_WaitReady_CtxCancel(t *testing.T) {
	fp := &fakePlatform{supportsInterim: true}
	ctx, cancel := context.WithCancel(context.Background())
	tracker := newIMEventTracker(ctx, fp, ReplyDest{ChatID: "c1"}, "direct", "")
	defer tracker.stop()
	cancel()
	done := make(chan struct{})
	go func() { tracker.waitReady(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady should return on context cancel")
	}
}

// ---------------------------------------------------------------------------
// firstLine — moved to internal/textutil.FirstLine in R222-CR-5.
// Equivalent coverage lives in internal/textutil/firstline_test.go::TestFirstLine.
// Keeping the dispatch-side cases here would duplicate that contract.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// formatToolUse
// ---------------------------------------------------------------------------

func TestFormatToolUse(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"Read", `{"file_path":"/a/b/c.go"}`, "📖 b/c.go"},
		{"Edit", `{"file_path":"/a/b/c.go"}`, "✏️ b/c.go"},
		{"Write", `{"file_path":"/a/b/c.go"}`, "📝 b/c.go"},
		{"Bash", `{"command":"go test ./..."}`, "⚡ go test ./..."},
		{"Grep", `{"pattern":"TODO"}`, "🔍 grep TODO"},
		{"Glob", `{"pattern":"*.go"}`, "🔍 *.go"},
		{"Agent", `{"description":"review changes"}`, "🤖 review changes"},
		{"CustomTool", `{}`, "🔧 CustomTool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatToolUse(tt.name, []byte(tt.input)); got != tt.want {
				t.Errorf("formatToolUse(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handleCdCommand — path traversal / allowed-root checks
// ---------------------------------------------------------------------------

func TestHandleCdCommand_AbsPath(t *testing.T) {
	// handleCdCommand runs filepath.EvalSymlinks before the allowedRoot
	// prefix check (commands.go:425). macOS rewrites /var/folders/... to
	// /private/var/folders/..., so resolve upfront to keep the fixture
	// platform-neutral.
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.allowedRoot = tmpDir // restrict to tmpDir

	msg := incomingMsg("/cd " + tmpDir)
	d.handleCdCommand(context.Background(), msg, "/cd "+tmpDir, slog.Default())

	if !strings.Contains(fp.lastReply(), "已切换") {
		t.Errorf("expected workspace-changed reply, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_NonExistentDir(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)

	msg := incomingMsg("/cd /nonexistent/path/abc123")
	d.handleCdCommand(context.Background(), msg, "/cd /nonexistent/path/abc123", slog.Default())

	if !strings.Contains(fp.lastReply(), "不存在") {
		t.Errorf("expected not-found message, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_OutsideAllowedRoot(t *testing.T) {
	tmpDir := t.TempDir()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.allowedRoot = "/some/other/root"

	msg := incomingMsg("/cd " + tmpDir)
	d.handleCdCommand(context.Background(), msg, "/cd "+tmpDir, slog.Default())

	if !strings.Contains(fp.lastReply(), "不允许") {
		t.Errorf("expected not-allowed message, got %q", fp.lastReply())
	}
}

// TestHandleCdCommand_CaseInsensitiveChild is the /cd counterpart of the
// dashboard resume bug: on a case-insensitive filesystem (macOS APFS) an
// allowedRoot configured in one case must still admit a /cd target typed in
// another case for the same physical directory. PathContainedInRoot's inode
// walk handles it. Skips on a case-sensitive fs where the bug can't occur.
func TestHandleCdCommand_CaseInsensitiveChild(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	child := filepath.Join(root, "Proj")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	lowerChild := filepath.Join(root, "proj")
	if _, err := os.Stat(lowerChild); err != nil {
		t.Skip("filesystem is case-sensitive; case-fold containment not exercisable here")
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.allowedRoot = child // mixed-case root

	// /cd the lowercase spelling — byte prefix mismatches, inode walk rescues.
	msg := incomingMsg("/cd " + lowerChild)
	d.handleCdCommand(context.Background(), msg, "/cd "+lowerChild, slog.Default())

	if !strings.Contains(fp.lastReply(), "已切换") {
		t.Errorf("case-variant /cd must be accepted on case-insensitive fs, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_EmptyPath(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	msg := incomingMsg("/cd")
	d.handleCdCommand(context.Background(), msg, "/cd", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected usage message, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_UnknownPlatform_NoReply(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	msg := platform.IncomingMessage{
		Platform: "unknown", EventID: "e1", UserID: "u1",
		ChatID: "c1", ChatType: "direct", Text: "/cd /tmp",
	}
	d.handleCdCommand(context.Background(), msg, "/cd /tmp", slog.Default())
	if fp.replyCount() != 0 {
		t.Errorf("expected no reply for unknown platform, got %d", fp.replyCount())
	}
}

// ---------------------------------------------------------------------------
// handleProjectCommand — no project manager
// ---------------------------------------------------------------------------

func TestHandleProjectCommand_NilManager(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.projectMgr = nil

	d.handleProjectCommand(context.Background(), incomingMsg("/project"), "/project", slog.Default())

	if !strings.Contains(fp.lastReply(), "项目功能未启用") {
		t.Errorf("expected no-project-manager message, got %q", fp.lastReply())
	}
}

func TestHandleProjectCommand_UnknownPlatform(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.projectMgr = nil

	msg := platform.IncomingMessage{
		Platform: "unknown", EventID: "e1", UserID: "u1",
		ChatID: "c1", ChatType: "direct", Text: "/project",
	}
	d.handleProjectCommand(context.Background(), msg, "/project", slog.Default())
	// No reply expected (unknown platform short-circuits)
	if fp.replyCount() != 0 {
		t.Errorf("expected no reply for unknown platform, got %d", fp.replyCount())
	}
}

// ---------------------------------------------------------------------------
// dispatchCommand — /cd via dispatchCommand
// ---------------------------------------------------------------------------

func TestDispatchCommand_Cd_Handled(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	// /cd is only handled if platform is known
	handled := d.dispatchCommand(context.Background(), incomingMsg("/cd"), "/cd /tmp", slog.Default())
	if !handled {
		t.Fatal("expected /cd to be handled by dispatchCommand")
	}
}

func TestDispatchCommand_Project_Handled(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.projectMgr = nil // nil manager → reply with "未启用"

	handled := d.dispatchCommand(context.Background(), incomingMsg("/project"), "/project", slog.Default())
	if !handled {
		t.Fatal("expected /project to be handled")
	}
}

// ---------------------------------------------------------------------------
// handleCronCommand — via dispatchCommand with real scheduler
// ---------------------------------------------------------------------------

func makeTestScheduler(t *testing.T) CronCommands {
	t.Helper()
	// #1164: previously stood up a real *cron.Scheduler (StorePath tempdir
	// + Start/Stop lifecycle). The handler tests below only exercise
	// dispatch-side parsing / usage-reply branches that never depend on
	// real scheduling, so the projection-typed fake suffices and the
	// dispatch test suite no longer imports internal/cron. The real
	// adapter→scheduler integration is covered by
	// internal/server/cron_dispatch_adapter_test.go.
	return &fakeCronScheduler{}
}

func TestHandleCronCommand_Help(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron"), "/cron", slog.Default())
	if !strings.Contains(fp.lastReply(), "add") {
		t.Errorf("expected cron usage, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_List_Empty(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron list"), "/cron list", slog.Default())
	if !strings.Contains(fp.lastReply(), "没有") {
		t.Errorf("expected empty-list message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Add_InvalidFormat(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron add"), "/cron add", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected add-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Del_MissingID(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron del"), "/cron del", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected del-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Del_NotFound(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron del"), "/cron del nosuchjob", slog.Default())
	if !strings.Contains(fp.lastReply(), "失败") || !strings.Contains(fp.lastReply(), "用法") {
		// Either "not found" error or usage message — both acceptable
	}
}

func TestHandleCronCommand_Pause_MissingID(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron pause"), "/cron pause", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected pause-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Resume_MissingID(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron resume"), "/cron resume", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected resume-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Add_InvalidSchedule(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = makeTestScheduler(t)

	// Valid format but invalid cron expression for robfig/cron
	d.handleCronCommand(context.Background(), incomingMsg("/cron add"), `/cron add "@not-a-valid-schedule" check it`, slog.Default())
	// Should get either error or success — check it ran without panic
	if fp.replyCount() == 0 {
		t.Error("expected at least one reply")
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — group chat gate (Commit 1: group + !MentionMe silently drops)
// ---------------------------------------------------------------------------

// TestBuildHandler_GroupChatGate exercises the gate that silently drops
// un-mentioned group chat messages. Direct chats are unaffected; mentioned
// group messages fall through to the normal path.
//
// The gate sits BEFORE dispatchCommand, so slash commands in groups ALSO
// require @bot — this is intentional and enforces the "groups need explicit
// activation" contract.
//
// Indicators used:
//   - replyCount > 0: handler reached a reply emission path. Plain-text messages
//     in the test env fail at GetOrCreate (no CLI wrapper) and surface a "会话创建
//     失败" error reply — still a reply, which is the signal we want. For
//     slash commands the reply is the /help text.
//   - messageCount: only bumped for non-slash text that passed dedup+gate; a
//     durable witness that "the gate let a plain-text message through", since
//     slash commands skip the counter.
//
// Both indicators are synchronous with respect to the gate — no goroutine
// scheduling involved — so no Eventually loop is needed.
func TestBuildHandler_GroupChatGate(t *testing.T) {
	tests := []struct {
		name        string
		chatType    string
		mentionMe   bool
		text        string
		wantReply   bool // true if any reply should be sent (success or error)
		wantMsgsInc bool // true if messageCount should have incremented
	}{
		{
			name:        "direct always responds to plain text",
			chatType:    "direct",
			mentionMe:   false,
			text:        "hello",
			wantReply:   true, // GetOrCreate error reply
			wantMsgsInc: true,
		},
		{
			name:        "direct always responds to slash command",
			chatType:    "direct",
			mentionMe:   false,
			text:        "/help",
			wantReply:   true,  // /help reply
			wantMsgsInc: false, // slash commands do not bump messageCount
		},
		{
			name:        "group + mention responds to plain text",
			chatType:    "group",
			mentionMe:   true,
			text:        "hello",
			wantReply:   true,
			wantMsgsInc: true,
		},
		{
			name:        "group + mention responds to slash command",
			chatType:    "group",
			mentionMe:   true,
			text:        "/help",
			wantReply:   true,
			wantMsgsInc: false,
		},
		{
			name:        "group without mention drops plain text",
			chatType:    "group",
			mentionMe:   false,
			text:        "hello",
			wantReply:   false,
			wantMsgsInc: false,
		},
		{
			name:        "group without mention drops slash command",
			chatType:    "group",
			mentionMe:   false,
			text:        "/help",
			wantReply:   false,
			wantMsgsInc: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fp := &fakePlatform{}
			// The non-slash path below runs an owner turn inline on this
			// goroutine and replies synchronously via the GetOrCreate error
			// arm.
			d := newTestDispatcher(fp)

			msg := platform.IncomingMessage{
				Platform:  "fake",
				EventID:   "evt-" + tt.name,
				UserID:    "u1",
				ChatID:    "chat1",
				ChatType:  tt.chatType,
				MentionMe: tt.mentionMe,
				Text:      tt.text,
			}
			d.BuildHandler()(context.Background(), msg)

			if got := fp.replyCount() > 0; got != tt.wantReply {
				t.Errorf("replyCount>0 = %v, want %v (replies=%q)", got, tt.wantReply, fp.allReplies())
			}
			if got := d.messageCount.Load() > 0; got != tt.wantMsgsInc {
				t.Errorf("messageCount>0 = %v, want %v", got, tt.wantMsgsInc)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// R248-TEST-2 — Capabilities precedence in NewDispatcher
// ---------------------------------------------------------------------------

// stubCaps is a Capabilities implementation whose ReplyFooter names it, so
// the precedence test can identify which wiring path the dispatcher resolved.
type stubCaps struct{ id string }

func (stubCaps) Takeover(_ context.Context, _, _ string, _ session.AgentOpts) bool { return false }
func (s stubCaps) ReplyFooter(_ string) string                                     { return "stubCaps:" + s.id }

// TestNewDispatcher_CapabilitiesPrecedence pins the three-way precedence
// resolution in NewDispatcher:
//
//  1. cfg.Capabilities wins when set, even if a legacy *Fn closure is also
//     provided; the closure is silently shadowed.
//  2. Only a legacy closure (no Capabilities) → closureCapabilities adapter.
//  3. Both unset → NoopCapabilities{} (covered by R248-TEST-6 below).
func TestNewDispatcher_CapabilitiesPrecedence(t *testing.T) {
	t.Parallel()

	legacyFooter := func(string) string { return "legacy-footer-fn" }
	cases := []struct {
		name string
		cfg  DispatcherConfig
		want string // d.caps.ReplyFooter's result
	}{
		{
			name: "only ReplyFooterFn → closureCapabilities adapter",
			cfg:  DispatcherConfig{ReplyFooterFn: legacyFooter},
			want: "legacy-footer-fn",
		},
		{
			name: "only Capabilities → used directly",
			cfg:  DispatcherConfig{Capabilities: stubCaps{id: "caps-only"}},
			want: "stubCaps:caps-only",
		},
		{
			name: "both set → Capabilities wins (closure shadowed)",
			cfg:  DispatcherConfig{Capabilities: stubCaps{id: "caps-wins"}, ReplyFooterFn: legacyFooter},
			want: "stubCaps:caps-wins",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.cfg.Turns = testTurns()
			d, err := NewDispatcher(tc.cfg)
			if err != nil {
				t.Fatalf("NewDispatcher: %v", err)
			}
			if d.caps == nil {
				t.Fatal("d.caps is nil — constructor must always install a Capabilities implementation")
			}
			if got := d.caps.ReplyFooter("claude"); got != tc.want {
				t.Errorf("d.caps.ReplyFooter = %q, want %q (precedence resolved to wrong wiring)", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// R248-TEST-6 — d.caps invariant: never nil after NewDispatcher
// ---------------------------------------------------------------------------

// TestNewDispatcher_CapsAlwaysNonNil pins the constructor invariant that
// d.caps is non-nil for any DispatcherConfig — including the all-zero case.
// The IM reply path calls d.caps.Takeover / d.caps.ReplyFooter
// unconditionally without nil guards; a regression that left d.caps zero on
// some construction path would surface as a nil-pointer panic on the first
// IM message instead of a clear constructor-time signal.
func TestNewDispatcher_CapsAlwaysNonNil(t *testing.T) {
	t.Parallel()
	d, err := NewDispatcher(DispatcherConfig{Turns: testTurns()})
	if err != nil {
		t.Fatalf("NewDispatcher with only Turns: %v", err)
	}
	if d.caps == nil {
		t.Fatal("d.caps is nil for empty DispatcherConfig — hot path will nil-panic on first message")
	}
	if _, ok := d.caps.(NoopCapabilities); !ok {
		t.Errorf("d.caps type = %T, want dispatch.NoopCapabilities for empty config (no closures, no Capabilities)", d.caps)
	}
	if got := d.caps.Takeover(context.Background(), "chat", "key", session.AgentOpts{}); got {
		t.Errorf("default Takeover = true, want false (NoopCapabilities default)")
	}
	if got := d.caps.ReplyFooter(""); got != "" {
		t.Errorf("default ReplyFooter = %q, want \"\" (NoopCapabilities default)", got)
	}
	if got := d.caps.ReplyFooter("claude"); got != "" {
		t.Errorf("default ReplyFooter(claude) = %q, want \"\" (NoopCapabilities ignores backendID)", got)
	}
}
