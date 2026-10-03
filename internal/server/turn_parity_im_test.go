package server

// IM-side rows of #3004's divergence table: what the dispatcher's owner loop,
// detached turns and slash commands do. Test names carry the row number. C2
// and D moved the IM pipeline onto the shared orchestrator without changing
// IM behaviour, so they rewired the helpers here but edited no assertion.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/turn"
)

const (
	parityPanicReply  = "处理异常，请稍后重试。"
	parityResetReply  = "对话已重置。"
	parityQueuedText  = "消息已收到，待当前回复完成后一并处理。"
	parityBusyText    = "正在处理上一条消息，请稍候..."
	parityMergedReply = "已合并到上一条回复。"
	parityNewSession  = "新会话已创建（之前的上下文已失效）。"
	parityReviewerKey = "parity:direct:chat1:code-reviewer"
)

func TestTurnParity01_IM_DrainTurnFailureReplies(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)

	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	turns.answer(okTurn("R1"))
	if r := h.plat.waitReply(t, "owner reply"); !strings.HasPrefix(r, "R1") {
		t.Fatalf("owner reply = %q, want R1", r)
	}
	if c := turns.turn(t, "drain turn", parityOutcome{Err: errParityBoom}); c.Text != "second" {
		t.Fatalf("drain turn text = %q, want the queued message", c.Text)
	}
	if r := h.plat.waitReply(t, "drain-turn error reply"); r != asyncErrorMessage(errParityBoom) {
		t.Fatalf("drain-turn failure reply = %q, want %q", r, asyncErrorMessage(errParityBoom))
	}
	h.waitDone(owner, "owner loop")
}

func TestTurnParity02_IM_FirstTurnFailureReplies(t *testing.T) {
	for _, mode := range []string{"collect", "interrupt"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, false)
			owner := h.imAsync("m1", "first")
			turns.turn(t, "owner turn", parityOutcome{Err: errParityBoom})
			if r := h.plat.waitReply(t, "error reply"); r != asyncErrorMessage(errParityBoom) {
				t.Fatalf("first-turn failure reply = %q, want %q", r, asyncErrorMessage(errParityBoom))
			}
			h.waitDone(owner, "owner loop")
		})
	}
}

func TestTurnParity03_IM_ResetCommandRecognition(t *testing.T) {
	h := newParityHarness(t, parityOpts{agentCommands: map[string]string{"review": "code-reviewer"}})
	turns := h.session(parityKey, false)
	cases := []struct{ text, want string }{
		{"/New", parityResetReply},
		{"/CLEAR  ", parityResetReply},
		{"/new review", "对话已重置 (code-reviewer)。"},
	}
	for i, tc := range cases {
		h.imSend("r"+string(rune('0'+i)), tc.text)
		if r := h.plat.waitReply(t, tc.text); r != tc.want {
			t.Errorf("%q: reply = %q, want %q", tc.text, r, tc.want)
		}
	}
	turns.noMoreTurns(t)
}

func TestTurnParity04_IM_ResetKeepsWorkspaceOverride(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	h.session(parityKey, false)
	dir := t.TempDir()
	h.router.SetWorkspace(parityChatKey, dir)
	h.imSend("m1", "/new")
	h.plat.waitReply(t, "reset reply")
	if got := h.router.Workspace(parityChatKey); got != dir {
		t.Fatalf("workspace override after IM /new = %q, want %q (IM keeps the /cd directory)", got, dir)
	}
}

func TestTurnParity05_IM_ResetClearsQueuedHourglass(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	if h.plat.addedFor("m2") != 1 {
		t.Fatal("queued IM message got no ⏳")
	}
	h.imSend("m3", "/new")
	if r := h.plat.waitReply(t, "reset reply"); r != parityResetReply {
		t.Fatalf("reset reply = %q", r)
	}
	if h.plat.removedFor("m2") != 1 {
		t.Fatal("IM /new did not clear the ⏳ of the message it dropped")
	}
	turns.answer(okTurn("R1"))
	h.plat.waitReply(t, "owner reply")
	h.waitDone(owner, "owner loop")
	turns.noMoreTurns(t)
}

func TestTurnParity06_IM_UrgentAnyModeCaseInsensitive(t *testing.T) {
	for _, mode := range []string{"collect", "interrupt", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, true)
			h.imSend("m1", "/URGENT  hi")
			c := turns.turn(t, "urgent turn", okTurn("U"))
			if c.Text != "hi" || c.Priority != "now" || !c.Passthrough {
				t.Fatalf("urgent turn = %+v, want text hi, priority now, via SendPassthrough", c)
			}
			if r := h.plat.waitReply(t, "urgent reply"); !strings.HasPrefix(r, "U") {
				t.Fatalf("urgent reply = %q", r)
			}
			h.imSend("m2", "/urgent")
			if r := h.plat.waitReply(t, "bare /urgent"); r != "用法：/urgent <紧急消息>（该消息会立即中断正在进行的回复）" {
				t.Fatalf("bare /urgent reply = %q, want the usage line", r)
			}
			turns.noMoreTurns(t)
		})
	}
}

func TestTurnParity07_IM_UrgentTargetsGeneralKey(t *testing.T) {
	h := newParityHarness(t, parityOpts{agentCommands: map[string]string{"review": "code-reviewer"}})
	general := h.session(parityKey, true)
	reviewer := h.session(parityReviewerKey, true)
	h.imSend("m1", "/urgent hi")
	general.turn(t, "urgent turn on the general key", okTurn("U"))
	h.plat.waitReply(t, "urgent reply")
	reviewer.noMoreTurns(t)
}

func TestTurnParity08_IM_PassthroughAndUrgentSignals(t *testing.T) {
	t.Run("passthrough mode uses SendPassthrough", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		h.imSend("m1", "hello")
		if c := turns.turn(t, "turn", okTurn("R")); !c.Passthrough || c.Priority != "" {
			t.Fatalf("passthrough-mode IM turn = %+v, want SendPassthrough with no priority", c)
		}
		h.plat.waitReply(t, "reply")
	})
	t.Run("collect mode uses Send", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, true)
		owner := h.imAsync("m1", "hello")
		if c := turns.turn(t, "turn", okTurn("R")); c.Passthrough {
			t.Fatalf("collect-mode IM turn = %+v, want Send", c)
		}
		h.waitDone(owner, "owner loop")
	})
	t.Run("urgent without passthrough interrupts then sends", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		h.imSend("m1", "/urgent hi")
		c := turns.turn(t, "urgent turn", okTurn("U"))
		if c.Passthrough || c.Text != "hi" || turns.interrupts() != 1 {
			t.Fatalf("urgent on a non-passthrough session = %+v, interrupts=%d; want InterruptViaControl then Send", c, turns.interrupts())
		}
		h.plat.waitReply(t, "reply")
	})
}

func TestTurnParity09_IM_DetachedTurnPanicIsRecovered(t *testing.T) {
	for _, tc := range []struct{ name, mode, text string }{
		{"passthrough turn", "passthrough", "hello"},
		{"urgent turn", "collect", "/urgent hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: tc.mode})
			turns := h.session(parityKey, true)
			h.imSend("m1", tc.text)
			turns.turn(t, "detached turn", parityOutcome{Panic: "parity: detached turn panic"})
			if r := h.plat.waitReply(t, "panic reply"); r != parityPanicReply {
				t.Fatalf("detached-turn panic reply = %q, want %q", r, parityPanicReply)
			}
		})
	}
}

// orderLog records cross-goroutine events in the order they happened.
type orderLog struct {
	mu sync.Mutex
	ev []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ev = append(o.ev, s)
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ev...)
}

// idleOrderSender logs NotifyIdle. The real Server exposes no seam for the
// order of NotifyIdle against panic handling, so row 10 rebuilds the
// dispatcher over the Server's own queue, platform and Capabilities with this
// Sender in front.
type idleOrderSender struct {
	turnSender
	log *orderLog
}

func (s idleOrderSender) NotifyIdle() {
	s.log.add("idle")
	s.turnSender.NotifyIdle()
}

// newSessionSender reports every GetOrCreate as a fresh session — the one
// status an injected session can never produce (row 17).
type newSessionSender struct{ turnSender }

func (s newSessionSender) GetOrCreate(ctx context.Context, key string, opts sessionview.AgentOpts) (turn.Session, sessionview.SessionStatus, error) {
	sess, _, err := s.turnSender.GetOrCreate(ctx, key, opts)
	return sess, sessionview.SessionNew, err
}

// takeoverRecordingCaps records Takeover calls on the way to the real caps.
type takeoverRecordingCaps struct {
	serverCaps
	log *orderLog
}

func (c takeoverRecordingCaps) Takeover(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool {
	c.log.add("takeover " + chatKey + " " + key)
	return c.serverCaps.Takeover(ctx, chatKey, key, opts)
}

// turnSender is the Sender buildDispatcher gives the IM orchestrator.
func (h *parityHarness) turnSender() turnSender {
	return turnSender{router: h.router, notify: h.hs.wiring.bcast}
}

// rebuiltIM builds a second dispatcher over h's Server state, its turns on a
// fresh Orchestrator with the Server's queue options, sender and caps
// swapped in.
func (h *parityHarness) rebuiltIM(t *testing.T, sender turn.Sender, caps dispatch.Capabilities) platform.MessageHandler {
	t.Helper()
	w := h.hs.wiring
	d, err := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:       h.router,
		Platforms:    h.srv.platforms,
		Agents:       w.agents,
		Resolver:     w.resolver,
		Turns:        turn.New(w.queue, sender),
		Dedup:        platform.NewDedup(64),
		Capabilities: caps,
		StopCtx:      h.srv.appCtx,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d.BuildHandler()
}

func TestTurnParity10_IM_NotifyIdleAfterPanicHandling(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	log := &orderLog{}
	h.plat.setOnReply(func(text string) { log.add("reply " + text) })
	im := h.rebuiltIM(t, idleOrderSender{h.turnSender(), log}, serverCaps{s: h.srv})

	done := make(chan struct{})
	go func() {
		defer close(done)
		im(h.imCtx, h.imMsg("m1", "first"))
	}()
	turns.turn(t, "owner turn", parityOutcome{Panic: "parity: owner turn panic"})
	h.waitDone(done, "owner loop")
	want := []string{"reply " + parityPanicReply, "idle"}
	if got := log.snapshot(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("IM owner-loop panic order = %q, want %q (NotifyIdle runs after the recover)", got, want)
	}
}

func TestTurnParity11_IM_PanicIsCounted(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	before := metrics.PanicRecoveredTotal.Value()
	owner := h.imAsync("m1", "first")
	turns.turn(t, "owner turn", parityOutcome{Panic: "parity: owner turn panic"})
	h.waitDone(owner, "owner loop")
	if got := metrics.PanicRecoveredTotal.Value() - before; got != 1 {
		t.Fatalf("PanicRecoveredTotal moved by %d on an IM owner-loop panic, want 1", got)
	}
}

func TestTurnParity12_IM_PanicClearsBatchAndRepliesOnce(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	h.imSend("m3", "third")
	turns.answer(okTurn("R1"))
	h.plat.waitReply(t, "owner reply")
	c := turns.next(t, "drain turn")
	if !strings.Contains(c.Text, "second") || !strings.Contains(c.Text, "third") {
		t.Fatalf("drain turn text = %q, want both queued messages", c.Text)
	}
	h.imSend("m4", "fourth") // queued behind the in-flight batch, dropped by the panic
	turns.answer(parityOutcome{Panic: "parity: drain turn panic"})
	if r := h.plat.waitReply(t, "panic reply"); r != parityPanicReply {
		t.Fatalf("panic reply = %q", r)
	}
	h.waitDone(owner, "owner loop")
	for _, id := range []string{"m2", "m3", "m4"} {
		if h.plat.removedFor(id) != 1 {
			t.Errorf("%s: ⏳ not cleared after the drain-turn panic", id)
		}
	}
	if n := replyMatching(h.plat.allReplies(), parityPanicReply); n != 1 {
		t.Errorf("panic replies = %d, want 1 (to the owner's chat)", n)
	}
	turns.noMoreTurns(t)
}

func TestTurnParity13_IM_ShutdownDrainClearsHourglass(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true, collect: time.Hour})
	turns := h.session(parityKey, false)
	ctx, cancel := context.WithCancel(h.imCtx)
	owner := h.imAsyncCtx(ctx, "m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	cancel()
	turns.answer(okTurn("R1"))
	h.plat.waitReply(t, "owner reply")
	h.waitDone(owner, "owner loop")
	if h.plat.removedFor("m2") != 1 {
		t.Fatal("IM owner exiting on ctx cancel did not clear the queued message's ⏳")
	}
	turns.noMoreTurns(t)
}

func TestTurnParity14_IM_EvictionClearsHourglass(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true, maxDepth: 1})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	h.imSend("m3", "third")
	if h.plat.removedFor("m2") != 1 {
		t.Fatal("evicted IM message kept its ⏳")
	}
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "third" {
		t.Fatalf("drain turn text = %q, want only the surviving message", c.Text)
	}
	h.waitDone(owner, "owner loop")
}

func TestTurnParity15_IM_DrainTimerRearms(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	turns.answer(okTurn("R1"))
	if c := turns.next(t, "first drain turn"); c.Text != "second" {
		t.Fatalf("first drain = %q", c.Text)
	}
	h.imSend("m3", "third")
	turns.answer(okTurn("R2"))
	if c := turns.turn(t, "second drain turn", okTurn("R3")); c.Text != "third" {
		t.Fatalf("second drain = %q", c.Text)
	}
	h.waitDone(owner, "owner loop")
	if h.plat.removedFor("m2") != 1 || h.plat.removedFor("m3") != 1 {
		t.Fatalf("drained IM messages kept their ⏳: removed m2=%d m3=%d, want 1 each",
			h.plat.removedFor("m2"), h.plat.removedFor("m3"))
	}
}

func TestTurnParity16_IM_InterruptModeInterruptsOnce(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "interrupt", reactor: true})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	h.imSend("m3", "third")
	if n := turns.interrupts(); n != 1 {
		t.Fatalf("interrupts = %d, want 1 (only the first follow-up of a turn interrupts)", n)
	}
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitDone(owner, "owner loop")
}

func TestTurnParity17_IM_FirstTurnTakeoverAndNewSessionNotice(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true, interim: true})
	turns := h.session(parityKey, false)
	log := &orderLog{}
	im := h.rebuiltIM(t, newSessionSender{h.turnSender()},
		takeoverRecordingCaps{serverCaps{s: h.srv}, log})

	done := make(chan struct{})
	go func() {
		defer close(done)
		im(h.imCtx, h.imMsg("m1", "first"))
	}()
	turns.next(t, "owner turn")
	if r := h.plat.waitReply(t, "new-session notice"); r != parityNewSession {
		t.Fatalf("first reply = %q, want the new-session notice", r)
	}
	im(h.imCtx, h.imMsg("m2", "second"))
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitDone(done, "owner loop")
	if got := log.snapshot(); len(got) != 1 || got[0] != "takeover "+parityChatKey+" "+parityKey {
		t.Fatalf("takeover calls = %q, want exactly one, for the first turn", got)
	}
	if n := replyMatching(h.plat.allReplies(), parityNewSession); n != 1 {
		t.Fatalf("new-session notices = %d, want 1 (drain turns skip it)", n)
	}
}

func TestTurnParity18_IM_MergeFollowerHint(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "passthrough"})
	turns := h.session(parityKey, true)
	h.imSend("m1", "follower")
	turns.turn(t, "follower turn", parityOutcome{Result: &clievent.SendResult{MergedCount: 2}})
	if r := h.plat.waitReply(t, "merge hint"); r != parityMergedReply {
		t.Fatalf("merge follower reply = %q, want %q", r, parityMergedReply)
	}
}

func TestTurnParity23_IM_DisabledQueueBusyNotice(t *testing.T) {
	h := newParityHarness(t, parityOpts{maxDepth: -1})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	if r := h.plat.waitReply(t, "busy notice"); r != parityBusyText {
		t.Fatalf("busy reply = %q, want %q", r, parityBusyText)
	}
	h.imSend("m3", "third") // inside the 3s cooldown: no second notice
	turns.answer(okTurn("R1"))
	h.plat.waitReply(t, "owner reply")
	h.waitDone(owner, "owner loop")
	if n := replyMatching(h.plat.allReplies(), parityBusyText); n != 1 {
		t.Fatalf("busy notices = %d, want 1 (rate-limited)", n)
	}
	turns.noMoreTurns(t)
}

func TestTurnParity24_IM_QueuedAck(t *testing.T) {
	t.Run("reactor platform gets an hourglass", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{reactor: true})
		turns := h.session(parityKey, false)
		owner := h.imAsync("m1", "first")
		turns.next(t, "owner turn")
		h.imSend("m2", "second")
		if h.plat.addedFor("m2") != 1 || len(h.plat.allReplies()) != 0 {
			t.Fatalf("queued ack: ⏳=%d replies=%q, want one ⏳ and no text", h.plat.addedFor("m2"), h.plat.allReplies())
		}
		turns.answer(okTurn("R1"))
		turns.turn(t, "drain turn", okTurn("R2"))
		h.waitDone(owner, "owner loop")
		if h.plat.removedFor("m2") != 1 {
			t.Fatal("drained IM message kept its ⏳")
		}
	})
	t.Run("plain platform gets one rate-limited text", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		owner := h.imAsync("m1", "first")
		turns.next(t, "owner turn")
		h.imSend("m2", "second")
		if r := h.plat.waitReply(t, "queued notice"); r != parityQueuedText {
			t.Fatalf("queued reply = %q", r)
		}
		h.imSend("m3", "third")
		turns.answer(okTurn("R1"))
		turns.turn(t, "drain turn", okTurn("R2"))
		h.waitDone(owner, "owner loop")
		if n := replyMatching(h.plat.allReplies(), parityQueuedText); n != 1 {
			t.Fatalf("queued notices = %d, want 1", n)
		}
	})
}

func TestTurnParity25_IM_AdmissionIgnoresEngineShutdown(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.engine().drain()
	owner := h.imAsync("m1", "first")
	turns.turn(t, "owner turn after engine drain", okTurn("R1"))
	if r := h.plat.waitReply(t, "owner reply"); !strings.HasPrefix(r, "R1") {
		t.Fatalf("reply = %q", r)
	}
	h.waitDone(owner, "owner loop")
}

func TestTurnParity26_IM_OptsResolvedOncePerOwnerLoop(t *testing.T) {
	agents := map[string]session.AgentOpts{"general": {}}
	h := newParityHarness(t, parityOpts{reactor: true, agents: agents})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	// Every reader of agents is parked or done; the drain turn reads after
	// the answer below.
	agents["general"] = session.AgentOpts{Model: "not a model"}
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn on the entry opts", okTurn("R2")); c.Text != "second" {
		t.Fatalf("drain turn = %q", c.Text)
	}
	h.waitDone(owner, "owner loop")
}

// Row 27: an IM key is never a cron key, so the autosave turnSender does for
// every entry since D never fires for an IM message.
func TestTurnParity27_IM_NoCronPromptAutosave(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	saver := &fakeCronPromptSaver{}
	turns := h.session(parityKey, false)
	sender := h.turnSender()
	sender.prompts = saver
	im := h.rebuiltIM(t, sender, serverCaps{s: h.srv})
	done := make(chan struct{})
	go func() {
		defer close(done)
		im(h.imCtx, h.imMsg("m1", "do Y"))
	}()
	turns.turn(t, "IM turn", okTurn("R"))
	h.waitDone(done, "owner loop")
	if saver.calls != 0 {
		t.Fatalf("SetJobPrompt calls = %d through the IM entry, want 0", saver.calls)
	}
}

// Row 28: a busy IM send takes the queue path and waits behind the
// owner; no path around the queue exists.
func TestTurnParity28_IM_QueuePathNotGuard(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "first")
	turns.next(t, "owner turn")
	h.imSend("m2", "second")
	if h.plat.addedFor("m2") != 1 || replyMatching(h.plat.allReplies(), parityBusyText) != 0 {
		t.Fatal("busy IM message did not take the queue path")
	}
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitDone(owner, "owner loop")
}
