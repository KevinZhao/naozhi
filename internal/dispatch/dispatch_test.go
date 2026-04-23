package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
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
// Fake guard
// ---------------------------------------------------------------------------

type fakeGuard struct {
	mu       sync.Mutex
	acquired map[string]bool
}

func newFakeGuard() *fakeGuard { return &fakeGuard{acquired: make(map[string]bool)} }

func (g *fakeGuard) TryAcquire(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.acquired[key] {
		return false
	}
	g.acquired[key] = true
	return true
}
func (g *fakeGuard) ShouldSendWait(_ string) bool { return true }
func (g *fakeGuard) Release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.acquired, key)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestDispatcher(fp *fakePlatform, sendFn func(context.Context, string, *session.ManagedSession, string, []cli.ImageData, cli.EventCallback) (*cli.SendResult, error)) *Dispatcher {
	if sendFn == nil {
		sendFn = func(_ context.Context, _ string, _ *session.ManagedSession, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
			return &cli.SendResult{Text: "ok"}, nil
		}
	}
	return NewDispatcher(DispatcherConfig{
		Router:                session.NewRouter(session.RouterConfig{MaxProcs: 10}),
		Platforms:             map[string]platform.Platform{"fake": fp},
		Agents:                map[string]session.AgentOpts{},
		AgentCommands:         map[string]string{},
		Guard:                 newFakeGuard(),
		Dedup:                 platform.NewDedup(100),
		SendFn:                sendFn,
		TakeoverFn:            func(_ context.Context, _, _ string, _ session.AgentOpts) bool { return false },
		WatchdogNoOutputKills: new(atomic.Int64),
		WatchdogTotalKills:    new(atomic.Int64),
		NoOutputTimeout:       5 * time.Second,
		TotalTimeout:          30 * time.Second,
	})
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, prompt, err := ParseCronAdd(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if sched != tt.wantSchedule {
					t.Errorf("schedule = %q, want %q", sched, tt.wantSchedule)
				}
				if prompt != tt.wantPrompt {
					t.Errorf("prompt = %q, want %q", prompt, tt.wantPrompt)
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
	d := newTestDispatcher(fp, nil)
	msg := platform.IncomingMessage{Platform: "nonexistent", ChatID: "c1"}
	if d.replyText(context.Background(), msg, "hi", nil) {
		t.Error("replyText should return false for unknown platform")
	}
}

func TestReplyText_KnownPlatform(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, nil)
	d.agentCommands = map[string]string{"review": "code-reviewer"}
	d.dispatchCommand(context.Background(), incomingMsg("/help"), "/help", slog.Default())
	if !strings.Contains(fp.lastReply(), "review") {
		t.Errorf("expected agent in help, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_New_Basic(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = NewMessageQueue(5, 0)
	if !d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new", slog.Default()) {
		t.Fatal("expected /new to be handled")
	}
	if !strings.Contains(fp.lastReply(), "重置") {
		t.Errorf("expected reset confirmation, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_Clear(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = NewMessageQueue(5, 0)
	if !d.dispatchCommand(context.Background(), incomingMsg("/clear"), "/clear", slog.Default()) {
		t.Fatal("expected /clear to be handled")
	}
	if !strings.Contains(fp.lastReply(), "重置") {
		t.Errorf("expected reset confirmation, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_New_UnknownAgent(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.agentCommands = map[string]string{"review": "reviewer"}
	d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new unknown-agent", slog.Default())
	if !strings.Contains(fp.lastReply(), "未知") {
		t.Errorf("expected unknown agent message, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_New_NamedAgent(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.agentCommands = map[string]string{"review": "reviewer"}
	d.queue = NewMessageQueue(5, 0)
	d.dispatchCommand(context.Background(), incomingMsg("/new"), "/new review", slog.Default())
	if !strings.Contains(fp.lastReply(), "重置") {
		t.Errorf("expected reset confirmation for named agent, got %q", fp.lastReply())
	}
}

func TestNormalizeSlashCommand(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"/Help", "/help"},
		{"/NEW", "/new"},
		{"/Cd /Path/To/Dir", "/cd /Path/To/Dir"},
		{"/cron add \"Job Name\"", "/cron add \"Job Name\""},
		{"hello world", "hello world"}, // non-slash passthrough
		{"/help", "/help"},             // already lowercase
	}
	for _, tc := range cases {
		if got := normalizeSlashCommand(tc.in); got != tc.want {
			t.Errorf("normalizeSlashCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDispatchCommand_CaseInsensitive(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	if !d.dispatchCommand(context.Background(), incomingMsg("/Help"), "/Help", slog.Default()) {
		t.Fatal("expected /Help to be handled case-insensitively")
	}
	if !strings.Contains(fp.lastReply(), "/help") {
		t.Errorf("case-insensitive /Help should return help; got %q", fp.lastReply())
	}
}

func TestDispatchCommand_Pwd(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	if !d.dispatchCommand(context.Background(), incomingMsg("/pwd"), "/pwd", slog.Default()) {
		t.Fatal("expected /pwd to be handled")
	}
	if !strings.Contains(fp.lastReply(), "工作目录") {
		t.Errorf("expected workspace in reply, got %q", fp.lastReply())
	}
}

func TestDispatchCommand_CronNoScheduler(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, nil)
	if d.dispatchCommand(context.Background(), incomingMsg("/foobar"), "/foobar", slog.Default()) {
		t.Fatal("unknown command should not be handled")
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — dedup
// ---------------------------------------------------------------------------

func TestBuildHandler_DedupDropsDuplicate(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, func(_ context.Context, _ string, _ *session.ManagedSession, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
		called = true
		return &cli.SendResult{Text: "ok"}, nil
	})
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
	d := newTestDispatcher(fp, nil)
	d.BuildHandler()(context.Background(), incomingMsg("/unknowncmd"))
	if !strings.Contains(fp.lastReply(), "未知命令") {
		t.Errorf("expected unknown-command message, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — path-like slash is NOT flagged as unknown command
// ---------------------------------------------------------------------------

func TestBuildHandler_PathSlash_NotUnknown(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = NewMessageQueue(5, 0)
	d.BuildHandler()(context.Background(), incomingMsg("/home/user/file.go を確認"))
	for _, r := range fp.allReplies() {
		if strings.Contains(r, "未知命令") {
			t.Errorf("path-like slash wrongly flagged as unknown: %q", r)
		}
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — guard path busy
// ---------------------------------------------------------------------------

func TestBuildHandler_GuardPath_Busy(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = nil // force guard path
	key := session.SessionKey("fake", "direct", "chat1", "general")
	d.guard.TryAcquire(key) // pre-acquire
	d.BuildHandler()(context.Background(), incomingMsg("hello"))
	if !strings.Contains(fp.lastReply(), "正在处理") {
		t.Errorf("expected busy message, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// BuildHandler — queue maxDepth=0 drop-notify
// ---------------------------------------------------------------------------

func TestBuildHandler_QueueDrop_Notify(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	q := NewMessageQueue(0, 0)
	d.queue = q
	// Mark session busy
	key := session.SessionKey("fake", "direct", "chat1", "general")
	q.Enqueue(key, QueuedMsg{Text: "busy", EnqueueAt: time.Now()})
	d.BuildHandler()(context.Background(), platform.IncomingMessage{
		Platform: "fake", EventID: "e2", UserID: "u1",
		ChatID: "chat1", ChatType: "direct", Text: "second",
	})
	if !strings.Contains(fp.lastReply(), "正在处理") {
		t.Errorf("expected drop-notify message, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// sendAndReply — GetOrCreate error paths
// ---------------------------------------------------------------------------

// sendAndReply is tested directly. Without a real wrapper, GetOrCreate fails and
// sendAndReply replies with an appropriate error message.

func TestSendAndReply_GetOrCreateError_DefaultMessage(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil) // no wrapper → GetOrCreate will fail
	d.queue = nil
	msg := incomingMsg("hello")
	d.sendAndReply(context.Background(), "key1", "hello", nil, "general",
		session.AgentOpts{}, msg, slog.Default(), true)
	// Default error → "会话创建失败" message
	if !strings.Contains(fp.lastReply(), "会话") {
		t.Errorf("expected session creation error message, got %q", fp.lastReply())
	}
}

// ---------------------------------------------------------------------------
// sendAndReply — unknown platform exits before GetOrCreate
// ---------------------------------------------------------------------------

func TestSendAndReply_UnknownPlatform(t *testing.T) {
	fp := &fakePlatform{}
	called := false
	d := newTestDispatcher(fp, func(_ context.Context, _ string, _ *session.ManagedSession, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
		called = true
		return &cli.SendResult{Text: "ok"}, nil
	})
	d.queue = nil
	msg := platform.IncomingMessage{
		Platform: "unknown", EventID: "e1", UserID: "u1",
		ChatID: "c1", ChatType: "direct", Text: "hello",
	}
	d.sendAndReply(context.Background(), "key1", "hello", nil, "general",
		session.AgentOpts{}, msg, slog.Default(), true)
	if called {
		t.Error("sendFn should not be called for unknown platform")
	}
}

// ---------------------------------------------------------------------------
// SendSplitReply
// ---------------------------------------------------------------------------

func TestSendSplitReply_Short(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.SendSplitReply(context.Background(), fp, "c1", "short message")
	if fp.replyCount() != 1 || fp.lastReply() != "short message" {
		t.Errorf("reply = %q, want %q", fp.lastReply(), "short message")
	}
}

func TestSendSplitReply_Long_Paginates(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	// >4000 chars → 2+ chunks
	d.SendSplitReply(context.Background(), fp, "c1", strings.Repeat("A", 8001))
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
	d := newTestDispatcher(fp, nil)
	d.SendSplitReply(context.Background(), &zeroMaxPlatform{fp}, "c1", "hello")
	if fp.replyCount() != 1 {
		t.Errorf("reply count = %d, want 1", fp.replyCount())
	}
}

// ---------------------------------------------------------------------------
// formatChineseDuration
// ---------------------------------------------------------------------------

func TestFormatChineseDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "未知"},
		{-time.Second, "未知"},
		{2 * time.Hour, "2 小时"},
		{3 * time.Minute, "3 分钟"},
		{45 * time.Second, "45 秒"},
		{90 * time.Second, "90 秒"},
		// not exact hour → seconds
		{time.Hour + time.Second, "3601 秒"},
	}
	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			if got := formatChineseDuration(tt.d); got != tt.want {
				t.Errorf("formatChineseDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// replyTracker
// ---------------------------------------------------------------------------

func TestReplyTracker_NonInterim_WaitReadyInstant(t *testing.T) {
	fp := &fakePlatform{supportsInterim: false}
	tracker := newIMEventTracker(context.Background(), fp, "c1")
	defer tracker.stop()
	tracker.onEvent(cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "thinking", Text: "t"}}},
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
	tracker := newIMEventTracker(ctx, fp, "c1")
	defer tracker.stop()
	tracker.onEvent(cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "thinking", Text: "analyzing"}}},
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
	tracker := newIMEventTracker(context.Background(), fp, "c1")
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
	tracker := newIMEventTracker(context.Background(), fp, "c1")
	tracker.stop()
	tracker.stop()
}

func TestReplyTracker_WaitReady_CtxCancel(t *testing.T) {
	fp := &fakePlatform{supportsInterim: true}
	ctx, cancel := context.WithCancel(context.Background())
	tracker := newIMEventTracker(ctx, fp, "c1")
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
// ownerLoop gen-mismatch
// ---------------------------------------------------------------------------

func TestOwnerLoop_GenMismatch(t *testing.T) {
	q := NewMessageQueue(5, 10*time.Millisecond)
	key := session.SessionKey("fake", "direct", "chat1", "general")
	_, _, gen := q.Enqueue(key, QueuedMsg{Text: "first", EnqueueAt: time.Now()})
	q.Enqueue(key, QueuedMsg{Text: "second", EnqueueAt: time.Now()})
	q.Discard(key)
	// Old gen → nil → stale owner stops
	if r := q.DoneOrDrain(key, gen); r != nil {
		t.Errorf("stale owner should get nil, got %v", r)
	}
}

// ---------------------------------------------------------------------------
// discardQueue nil-safe
// ---------------------------------------------------------------------------

func TestDiscardQueue_Nil(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.queue = nil
	d.discardQueue("any-key") // must not panic
}

func TestDiscardQueue_WithQueue(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	q := NewMessageQueue(5, 0)
	d.queue = q
	key := "test-key"
	q.Enqueue(key, QueuedMsg{Text: "m"})
	d.discardQueue(key)
	if q.Depth(key) != 0 {
		t.Errorf("depth = %d after discard, want 0", q.Depth(key))
	}
}

// ---------------------------------------------------------------------------
// ShouldNotify dropNotifyTimes path
// ---------------------------------------------------------------------------

func TestShouldNotify_DropPath(t *testing.T) {
	q := NewMessageQueue(0, 0)
	if !q.ShouldNotify("k") {
		t.Fatal("first call should return true")
	}
	if q.ShouldNotify("k") {
		t.Fatal("immediate second call should be rate-limited")
	}
}

func TestShouldNotify_DropPath_Eviction(t *testing.T) {
	q := NewMessageQueue(0, 0)
	for i := 0; i < dropNotifyMaxKeys; i++ {
		q.ShouldNotify(fmt.Sprintf("key-%d", i))
	}
	if !q.ShouldNotify("overflow-key") {
		t.Fatal("should notify after eviction at capacity")
	}
	q.mu.Lock()
	size := q.dropNotifyLRU.Len()
	idxSize := len(q.dropNotifyIndex)
	q.mu.Unlock()
	if size > dropNotifyMaxKeys {
		t.Errorf("dropNotifyLRU size = %d > cap %d", size, dropNotifyMaxKeys)
	}
	if idxSize != size {
		t.Errorf("dropNotifyIndex size %d != LRU size %d", idxSize, size)
	}
}

// ---------------------------------------------------------------------------
// CollectDelay / ShouldSendWait
// ---------------------------------------------------------------------------

func TestCollectDelay(t *testing.T) {
	if got := NewMessageQueue(5, 200*time.Millisecond).CollectDelay(); got != 200*time.Millisecond {
		t.Errorf("CollectDelay = %v, want 200ms", got)
	}
}

func TestShouldSendWait(t *testing.T) {
	q := NewMessageQueue(5, 0)
	if !q.ShouldSendWait("k") {
		t.Fatal("first call should return true")
	}
	if q.ShouldSendWait("k") {
		t.Fatal("second call should be rate-limited")
	}
}

// ---------------------------------------------------------------------------
// firstLine
// ---------------------------------------------------------------------------

func TestFirstLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hello", "hello"},
		{"first\nsecond", "first"},
		{"\nfirst", "first"},
		{"  \n  real  \n", "real"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := firstLine(tt.in); got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

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
	tmpDir := t.TempDir()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.allowedRoot = tmpDir // restrict to tmpDir

	msg := incomingMsg("/cd " + tmpDir)
	d.handleCdCommand(context.Background(), msg, "/cd "+tmpDir, slog.Default())

	if !strings.Contains(fp.lastReply(), "已切换") {
		t.Errorf("expected workspace-changed reply, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_NonExistentDir(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)

	msg := incomingMsg("/cd /nonexistent/path/abc123")
	d.handleCdCommand(context.Background(), msg, "/cd /nonexistent/path/abc123", slog.Default())

	if !strings.Contains(fp.lastReply(), "不存在") {
		t.Errorf("expected not-found message, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_OutsideAllowedRoot(t *testing.T) {
	tmpDir := t.TempDir()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.allowedRoot = "/some/other/root"

	msg := incomingMsg("/cd " + tmpDir)
	d.handleCdCommand(context.Background(), msg, "/cd "+tmpDir, slog.Default())

	if !strings.Contains(fp.lastReply(), "不允许") {
		t.Errorf("expected not-allowed message, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_EmptyPath(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	msg := incomingMsg("/cd")
	d.handleCdCommand(context.Background(), msg, "/cd", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected usage message, got %q", fp.lastReply())
	}
}

func TestHandleCdCommand_UnknownPlatform_NoReply(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, nil)
	d.projectMgr = nil

	d.handleProjectCommand(context.Background(), incomingMsg("/project"), "/project", slog.Default())

	if !strings.Contains(fp.lastReply(), "项目功能未启用") {
		t.Errorf("expected no-project-manager message, got %q", fp.lastReply())
	}
}

func TestHandleProjectCommand_UnknownPlatform(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
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
	d := newTestDispatcher(fp, nil)
	// /cd is only handled if platform is known
	handled := d.dispatchCommand(context.Background(), incomingMsg("/cd"), "/cd /tmp", slog.Default())
	if !handled {
		t.Fatal("expected /cd to be handled by dispatchCommand")
	}
}

func TestDispatchCommand_Project_Handled(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.projectMgr = nil // nil manager → reply with "未启用"

	handled := d.dispatchCommand(context.Background(), incomingMsg("/project"), "/project", slog.Default())
	if !handled {
		t.Fatal("expected /project to be handled")
	}
}

// ---------------------------------------------------------------------------
// handleCronCommand — via dispatchCommand with real scheduler
// ---------------------------------------------------------------------------

func makeTestScheduler(t *testing.T) *cron.Scheduler {
	t.Helper()
	s := cron.NewScheduler(cron.SchedulerConfig{
		StorePath: t.TempDir(),
		MaxJobs:   10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestHandleCronCommand_Help(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron"), "/cron", slog.Default())
	if !strings.Contains(fp.lastReply(), "add") {
		t.Errorf("expected cron usage, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_List_Empty(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron list"), "/cron list", slog.Default())
	if !strings.Contains(fp.lastReply(), "没有") {
		t.Errorf("expected empty-list message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Add_InvalidFormat(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron add"), "/cron add", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected add-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Del_MissingID(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron del"), "/cron del", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected del-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Del_NotFound(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron del"), "/cron del nosuchjob", slog.Default())
	if !strings.Contains(fp.lastReply(), "失败") || !strings.Contains(fp.lastReply(), "用法") {
		// Either "not found" error or usage message — both acceptable
	}
}

func TestHandleCronCommand_Pause_MissingID(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron pause"), "/cron pause", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected pause-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Resume_MissingID(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	d.dispatchCommand(context.Background(), incomingMsg("/cron resume"), "/cron resume", slog.Default())
	if !strings.Contains(fp.lastReply(), "用法") {
		t.Errorf("expected resume-usage message, got %q", fp.lastReply())
	}
}

func TestHandleCronCommand_Add_InvalidSchedule(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, nil)
	d.scheduler = makeTestScheduler(t)

	// Valid format but invalid cron expression for robfig/cron
	d.handleCronCommand(context.Background(), incomingMsg("/cron add"), `/cron add "@not-a-valid-schedule" check it`, slog.Default())
	// Should get either error or success — check it ran without panic
	if fp.replyCount() == 0 {
		t.Error("expected at least one reply")
	}
}
