package server

// Dashboard-side rows of #3004's divergence table: what sessionSend, the
// engine's owner loop and its detached passthrough turns do today. D flips the
// rows the table marks "D"; each flip edits the assertion here in the same PR
// and names the row.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

func TestTurnParity01_Dash_DrainTurnFailureIsSilent(t *testing.T) {
	t.Run("send fails", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		ws := h.ws()
		ws.send("w1", "first")
		turns.next(t, "owner turn")
		ws.send("w2", "second")
		if s := ws.ack(t, "w2"); s != "queued" {
			t.Fatalf("w2 ack = %q", s)
		}
		turns.answer(okTurn("R1"))
		turns.turn(t, "drain turn", parityOutcome{Err: errParityBoom})
		h.waitEngineIdle()
		if acks := ws.errorAcks(); len(acks) != 0 {
			t.Fatalf("drain-turn failure produced error acks %+v, want none", acks)
		}
	})
	t.Run("session lookup fails", func(t *testing.T) {
		agents := map[string]session.AgentOpts{"general": {}}
		h := newParityHarness(t, parityOpts{agents: agents})
		turns := h.session(parityKey, false)
		ws := h.ws()
		ws.send("w1", "first")
		turns.next(t, "owner turn")
		ws.send("w2", "second")
		if s := ws.ack(t, "w2"); s != "queued" {
			t.Fatalf("w2 ack = %q", s)
		}
		agents["general"] = session.AgentOpts{Model: "not a model"} // the drain turn's GetOrCreate rejects it
		turns.answer(okTurn("R1"))
		h.waitEngineIdle()
		turns.noMoreTurns(t)
		if acks := ws.errorAcks(); len(acks) != 0 {
			t.Fatalf("drain-turn GetOrCreate failure produced error acks %+v, want none", acks)
		}
	})
}

func TestTurnParity02_Dash_FirstTurnFailureReportedOnlyInPassthrough(t *testing.T) {
	for _, mode := range []string{"collect", "interrupt"} {
		t.Run(mode+" is silent", func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, false)
			ws := h.ws()
			ws.send("w1", "first")
			turns.turn(t, "owner turn", parityOutcome{Err: errParityBoom})
			h.waitEngineIdle()
			if acks := ws.errorAcks(); len(acks) != 0 {
				t.Fatalf("%s first-turn failure produced error acks %+v, want none", mode, acks)
			}
		})
	}
	t.Run("passthrough reports", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		ws := h.ws()
		ws.send("w1", "first")
		turns.turn(t, "turn", parityOutcome{Err: errParityBoom})
		f := ws.waitFor(t, "error ack", func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
		if f.ID != "w1" || f.Error != asyncErrorMessage(errParityBoom) {
			t.Fatalf("passthrough failure ack = %+v", f)
		}
	})
}

func TestTurnParity03_Dash_ResetOnlyOnExactCommand(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "/new review")
	if s := ws.ack(t, "w1"); s != "accepted" {
		t.Fatalf("/new <arg> ack = %q, want accepted (sent as text)", s)
	}
	if c := turns.turn(t, "turn", okTurn("R")); c.Text != "/new review" {
		t.Fatalf("turn text = %q, want the literal command", c.Text)
	}
	h.waitEngineIdle()
	for i, text := range []string{" /CLEAR ", "/New"} {
		id := "r" + string(rune('0'+i))
		ws.send(id, text)
		if s := ws.ack(t, id); s != "reset" {
			t.Errorf("%q ack = %q, want reset", text, s)
		}
	}
	turns.noMoreTurns(t)
}

func TestTurnParity04_Dash_ResetDiscardsWorkspaceOverride(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	h.session(parityKey, false)
	h.router.SetWorkspace(parityChatKey, t.TempDir())
	ws := h.ws()
	ws.send("w1", "/new")
	ws.ack(t, "w1")
	if got := h.router.Workspace(parityChatKey); got != "" {
		t.Fatalf("workspace override after dashboard /new = %q, want it discarded", got)
	}
}

func TestTurnParity06_Dash_UrgentOnlyInPassthroughCaseSensitive(t *testing.T) {
	t.Run("collect sends it as text", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, true)
		ws := h.ws()
		ws.send("w1", "/urgent hi")
		if c := turns.turn(t, "turn", okTurn("R")); c.Text != "/urgent hi" || c.Priority != "" || c.Passthrough {
			t.Fatalf("collect /urgent turn = %+v, want the literal text through Send", c)
		}
		h.waitEngineIdle()
	})
	t.Run("passthrough", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		ws := h.ws()
		for i, tc := range []struct{ in, text, priority string }{
			{"/urgent hi", "hi", "now"},
			{"/URGENT hi", "/URGENT hi", ""},
			{"/urgent", "/urgent", ""},
		} {
			ws.send("w"+string(rune('0'+i)), tc.in)
			c := turns.turn(t, tc.in, okTurn("R"))
			if c.Text != tc.text || c.Priority != tc.priority {
				t.Errorf("%q: turn = %+v, want text %q priority %q", tc.in, c, tc.text, tc.priority)
			}
			h.waitEngineIdle()
		}
	})
}

func TestTurnParity07_Dash_UrgentTargetsCurrentKey(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "passthrough"})
	general := h.session(parityKey, true)
	reviewer := h.session(parityReviewerKey, true)
	ws := h.ws()
	ws.sendTo(parityReviewerKey, "w1", "/urgent hi")
	if c := reviewer.turn(t, "urgent turn on the addressed key", okTurn("U")); c.Priority != "now" {
		t.Fatalf("turn = %+v", c)
	}
	h.waitEngineIdle()
	general.noMoreTurns(t)
}

func TestTurnParity08_Dash_PassthroughSignal(t *testing.T) {
	for _, tc := range []struct {
		name, mode   string
		capable      bool
		wantPassthru bool
	}{
		{"passthrough mode, capable session", "passthrough", true, true},
		{"passthrough mode, Send-only session", "passthrough", false, false},
		{"collect mode", "collect", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: tc.mode})
			turns := h.session(parityKey, tc.capable)
			ws := h.ws()
			ws.send("w1", "hello")
			if c := turns.turn(t, "turn", okTurn("R")); c.Passthrough != tc.wantPassthru || c.Priority != "" {
				t.Fatalf("turn = %+v, want passthrough=%v", c, tc.wantPassthru)
			}
			h.waitEngineIdle()
		})
	}
}

// parityChildEnv selects the child half of the row-9 crash test.
const parityChildEnv = "NAOZHI_TURN_PARITY_CHILD"

// TestTurnParity09_Dash_DetachedTurnPanicCrashesProcess pins that a panic in a
// dashboard passthrough turn is NOT recovered: it takes the process down. The
// panic runs in a child copy of this test binary so the crash is observable.
func TestTurnParity09_Dash_DetachedTurnPanicCrashesProcess(t *testing.T) {
	if os.Getenv(parityChildEnv) == "row09" {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		ws := h.ws()
		ws.send("w1", "hello")
		turns.turn(t, "turn", parityOutcome{Panic: "parity-row09-unrecovered"})
		// The goroutine's deferred release runs while the panic unwinds, so an
		// idle engine is no proof of survival. The expected event is this
		// process dying; parityWait is its deadline, as for every wait here.
		h.waitEngineIdle()
		<-time.After(parityWait)
		t.Log("parity-row09-survived")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTurnParity09_Dash_DetachedTurnPanicCrashesProcess$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), parityChildEnv+"=row09")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil || strings.Contains(out.String(), "--- PASS: TestTurnParity09_Dash") {
		t.Fatalf("child survived a dashboard passthrough-turn panic (err=%v):\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "panic: parity-row09-unrecovered") {
		t.Fatalf("child failed without the scripted panic:\n%s", out.String())
	}
}

// idleOrderEngineRouter logs NotifyIdle in front of the real router (row 10;
// see idleOrderDispatchRouter for why the engine is rebuilt).
type idleOrderEngineRouter struct {
	*session.Router
	log *orderLog
}

func (r idleOrderEngineRouter) NotifyIdle() {
	r.log.add("idle")
	r.Router.NotifyIdle()
}

// rebuiltEngine builds a second send engine over h's queue, guard and
// broadcaster with router swapped in.
func (h *parityHarness) rebuiltEngine(t *testing.T, router sendEngineRouter) *sendEngine {
	t.Helper()
	w := h.hs.wiring
	e := newSendEngine(sendEngineOpts{
		Queue:    w.msgQueue,
		Guard:    w.sessionGuard,
		Ctx:      h.srv.appCtx,
		Router:   router,
		Resolver: w.resolver,
		Agents:   w.agents,
		Notify:   w.bcast,
	})
	t.Cleanup(e.drain)
	return e
}

func TestTurnParity10_Dash_NotifyIdleBeforePanicHandling(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	log := &orderLog{}
	e := h.rebuiltEngine(t, idleOrderEngineRouter{h.router, log})
	if _, status, err := e.sessionSend(sendParams{Key: parityKey, Text: "first"}, func(_ error, msg string) {
		log.add("async " + msg)
	}); err != nil || status != sendAckAccepted {
		t.Fatalf("sessionSend = %q, %v", status, err)
	}
	turns.turn(t, "owner turn", parityOutcome{Panic: "parity: owner turn panic"})
	waitEngineIdle(t, e)
	want := []string{"idle", "async " + parityPanicReply}
	if got := log.snapshot(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("dashboard owner-loop panic order = %q, want %q (NotifyIdle runs before the recover)", got, want)
	}
}

func TestTurnParity11_Dash_PanicIsNotCounted(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	before := metrics.PanicRecoveredTotal.Value()
	ws.send("w1", "first")
	turns.turn(t, "owner turn", parityOutcome{Panic: "parity: owner turn panic"})
	ws.waitFor(t, "panic ack", func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
	h.waitEngineIdle()
	if got := metrics.PanicRecoveredTotal.Value() - before; got != 0 {
		t.Fatalf("PanicRecoveredTotal moved by %d on a dashboard owner-loop panic, want 0", got)
	}
}

func TestTurnParity12_Dash_DrainPanicReportedToFirstSend(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	ws.ack(t, "w2")
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", parityOutcome{Panic: "parity: drain turn panic"})
	h.waitEngineIdle()
	acks := ws.errorAcks()
	if len(acks) != 1 || acks[0].ID != "w1" || acks[0].Error != parityPanicReply {
		t.Fatalf("error acks after a drain-turn panic = %+v, want one %q for w1 (the already-finished first send)", acks, parityPanicReply)
	}
}

func TestTurnParity13_Dash_ShutdownDrainDiscardsQueue(t *testing.T) {
	h := newParityHarness(t, parityOpts{collect: time.Hour})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	if s := ws.ack(t, "w2"); s != "queued" {
		t.Fatalf("w2 ack = %q", s)
	}
	h.engine().cancel()
	turns.answer(okTurn("R1"))
	h.waitEngineIdle()
	turns.noMoreTurns(t)
	q := h.hs.wiring.msgQueue
	if isOwner, _, _, _, _ := q.Enqueue(parityKey, dispatch.QueuedMsg{Text: "probe"}); !isOwner {
		t.Fatal("dashboard owner exiting on ctx cancel left the key owned")
	}
	q.Discard(parityKey)
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("discarded dashboard message got error acks %+v, want none", acks)
	}
}

func TestTurnParity14_Dash_EvictedSendIsNotTold(t *testing.T) {
	h := newParityHarness(t, parityOpts{maxDepth: 1})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	ws.send("w3", "third")
	if a, b := ws.ack(t, "w2"), ws.ack(t, "w3"); a != "queued" || b != "queued" {
		t.Fatalf("acks = %q, %q", a, b)
	}
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "third" {
		t.Fatalf("drain turn = %q, want only the surviving message", c.Text)
	}
	h.waitEngineIdle()
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("evicted send got %+v, want nothing", acks)
	}
}

func TestTurnParity15_Dash_DrainTimerRearms(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	turns.answer(okTurn("R1"))
	if c := turns.next(t, "first drain turn"); c.Text != "second" {
		t.Fatalf("first drain = %q", c.Text)
	}
	ws.send("w3", "third")
	turns.answer(okTurn("R2"))
	if c := turns.turn(t, "second drain turn", okTurn("R3")); c.Text != "third" {
		t.Fatalf("second drain = %q", c.Text)
	}
	h.waitEngineIdle()
}

func TestTurnParity16_Dash_InterruptModeInterruptsOnce(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "interrupt"})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	ws.send("w3", "third")
	if n := turns.interrupts(); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitEngineIdle()
}

// newSessionEngineRouter reports every GetOrCreate as a fresh session (row 17).
type newSessionEngineRouter struct{ *session.Router }

func (r newSessionEngineRouter) GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
	s, _, err := r.Router.GetOrCreate(ctx, key, opts)
	return s, session.SessionNew, err
}

func TestTurnParity17_Dash_NoTakeoverNoNewSessionNotice(t *testing.T) {
	h := newParityHarness(t, parityOpts{interim: true})
	turns := h.session(parityKey, false)
	e := h.rebuiltEngine(t, newSessionEngineRouter{h.router})
	var asyncMsgs []string
	if _, _, err := e.sessionSend(sendParams{Key: parityKey, Text: "first"}, func(_ error, msg string) {
		asyncMsgs = append(asyncMsgs, msg)
	}); err != nil {
		t.Fatal(err)
	}
	turns.turn(t, "owner turn on a fresh session", okTurn("R1"))
	waitEngineIdle(t, e)
	if r := h.plat.allReplies(); len(r) != 0 || len(asyncMsgs) != 0 {
		t.Fatalf("dashboard first turn on a new session: IM replies %q, async %q; want neither", r, asyncMsgs)
	}
}

func TestTurnParity18_Dash_MergeFollowerIsSilent(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "passthrough"})
	turns := h.session(parityKey, true)
	ws := h.ws()
	ws.send("w1", "follower")
	turns.turn(t, "follower turn", parityOutcome{Result: &clievent.SendResult{MergedCount: 2}})
	h.waitEngineIdle()
	if acks := ws.errorAcks(); len(acks) != 0 || len(h.plat.allReplies()) != 0 {
		t.Fatalf("merge follower: error acks %+v, IM replies %q; want neither", acks, h.plat.allReplies())
	}
}

func TestTurnParity23_Dash_DisabledQueueBusyAck(t *testing.T) {
	h := newParityHarness(t, parityOpts{maxDepth: -1})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	if s := ws.ack(t, "w2"); s != "busy" {
		t.Fatalf("w2 ack = %q, want busy", s)
	}
	turns.answer(okTurn("R1"))
	h.waitEngineIdle()
	turns.noMoreTurns(t)
}

func TestTurnParity24_Dash_QueuedAck(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	if s := ws.ack(t, "w1"); s != "accepted" {
		t.Fatalf("w1 ack = %q", s)
	}
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	if s := ws.ack(t, "w2"); s != "queued" {
		t.Fatalf("w2 ack = %q, want queued", s)
	}
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitEngineIdle()
}

func TestTurnParity25_Dash_ShutdownAdmissionBusy(t *testing.T) {
	for _, mode := range []string{"collect", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, true)
			ws := h.ws()
			h.engine().drain()
			ws.send("w1", "first")
			if s := ws.ack(t, "w1"); s != "busy" {
				t.Fatalf("ack during shutdown = %q, want busy", s)
			}
			turns.noMoreTurns(t)
			q := h.hs.wiring.msgQueue
			if isOwner, _, _, _, _ := q.Enqueue(parityKey, dispatch.QueuedMsg{Text: "probe"}); !isOwner {
				t.Fatal("a send refused during shutdown left the key owned")
			}
			q.Discard(parityKey)
		})
	}
}

func TestTurnParity26_Dash_OptsResolvedPerTurn(t *testing.T) {
	agents := map[string]session.AgentOpts{"general": {}}
	h := newParityHarness(t, parityOpts{agents: agents})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	if s := ws.ack(t, "w2"); s != "queued" {
		t.Fatalf("w2 ack = %q", s)
	}
	agents["general"] = session.AgentOpts{Model: "not a model"}
	turns.answer(okTurn("R1"))
	h.waitEngineIdle()
	turns.noMoreTurns(t) // the drain turn re-resolved opts and its GetOrCreate refused them
}

func TestTurnParity27_Dash_CronPromptAutosave(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	saver := &fakeCronPromptSaver{}
	h.engine().scheduler = saver
	cronKey := sessionkey.CronKey("job1")
	turns := h.session(cronKey, false)
	ws := h.ws()
	ws.sendTo(cronKey, "w1", "do X")
	turns.turn(t, "successful cron turn", okTurn("R"))
	h.waitEngineIdle()
	ws.sendTo(cronKey, "w2", "do Z")
	turns.turn(t, "failed cron turn", parityOutcome{Err: errParityBoom})
	h.waitEngineIdle()
	if saver.calls != 1 || saver.lastJobID != "job1" || saver.lastPrompt != "do X" {
		t.Fatalf("SetJobPrompt calls=%d job=%q prompt=%q, want one call for the successful turn", saver.calls, saver.lastJobID, saver.lastPrompt)
	}
}

func TestTurnParity28_Dash_QueuePathNotLegacy(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	if !h.hs.wiring.sessionGuard.TryAcquire(parityKey) {
		t.Fatal("session.Guard is held during a dashboard owner turn: the legacy path is in use")
	}
	h.hs.wiring.sessionGuard.Release(parityKey)
	if s := h.httpSend(t, "second"); s != "queued" {
		t.Fatalf("HTTP send while busy = %q, want queued", s)
	}
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitEngineIdle()
	if n := h.engine().LegacySendInvokes(); n != 0 {
		t.Fatalf("LegacySendInvokes = %d, want 0 on a production-built Server", n)
	}
}
