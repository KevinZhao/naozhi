package server

// Dashboard-side rows of #3004's divergence table. Since D the dashboard's
// turns run on turn.Orchestrator like IM's, through wsOrigin / httpOrigin; the
// rows the table marks "D" were flipped to their new values here, each test
// name saying what the row is now.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
)

// onlyErrorAck fails t unless acks is exactly one error ack, for id, saying msg.
func onlyErrorAck(t *testing.T, acks []parityFrame, id, msg string) {
	t.Helper()
	if len(acks) != 1 || acks[0].ID != id || acks[0].Error != msg {
		t.Fatalf("error acks = %+v, want exactly one %q for %s", acks, msg, id)
	}
}

// Row 1 flipped in D: a merged follow-up turn's failure reaches the sends it
// carried, not the owner's already-answered one.
func TestTurnParity01_Dash_DrainTurnFailureReported(t *testing.T) {
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
		onlyErrorAck(t, ws.errorAcks(), "w2", asyncErrorMessage(errParityBoom))
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
		acks := ws.errorAcks()
		if len(acks) != 1 || acks[0].ID != "w2" || acks[0].Error == "" {
			t.Fatalf("drain-turn GetOrCreate failure: error acks %+v, want one for w2", acks)
		}
	})
}

// Row 2 flipped in D: a first turn's failure is reported in every mode.
func TestTurnParity02_Dash_FirstTurnFailureReported(t *testing.T) {
	for _, mode := range []string{"collect", "interrupt", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, mode == "passthrough")
			ws := h.ws()
			ws.send("w1", "first")
			turns.turn(t, "first turn", parityOutcome{Err: errParityBoom})
			h.waitEngineIdle()
			onlyErrorAck(t, ws.errorAcks(), "w1", asyncErrorMessage(errParityBoom))
		})
	}
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

// Row 4: a dashboard /new drops the workspace override and refreshes every
// tab's session list.
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
	ws.waitFor(t, "sessions_update after /new", func(f parityFrame) bool { return f.Type == "sessions_update" })
}

// Row 6 flipped in D: /urgent is recognised in every mode, case-insensitively,
// and a bare /urgent is a validation error that starts no turn.
func TestTurnParity06_Dash_UrgentAnyModeCaseInsensitive(t *testing.T) {
	for _, mode := range []string{"collect", "interrupt", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: mode})
			turns := h.session(parityKey, true)
			ws := h.ws()
			for i, in := range []string{"/urgent hi", "/URGENT  hi"} {
				id := "w" + string(rune('0'+i))
				ws.send(id, in)
				if s := ws.ack(t, id); s != "accepted" {
					t.Fatalf("%q ack = %q, want accepted (a detached turn runs now)", in, s)
				}
				c := turns.turn(t, in, okTurn("R"))
				if c.Text != "hi" || c.Priority != "now" || !c.Passthrough {
					t.Errorf("%q: turn = %+v, want text hi, priority now, via SendPassthrough", in, c)
				}
				h.waitEngineIdle()
			}
			ws.send("wb", "/urgent")
			if f := ws.waitFor(t, "bare /urgent ack", func(f parityFrame) bool { return f.Type == "send_ack" && f.ID == "wb" }); f.Status != "error" || f.Error != "用法：/urgent <紧急消息>" {
				t.Fatalf("bare /urgent ack = %+v, want the usage error", f)
			}
			turns.noMoreTurns(t)
		})
	}
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

// Row 9 flipped in D: a detached dashboard turn's panic is recovered and
// reported, and the engine goes on serving.
func TestTurnParity09_Dash_DetachedTurnPanicIsRecovered(t *testing.T) {
	for _, tc := range []struct{ name, mode, text string }{
		{"passthrough turn", "passthrough", "hello"},
		{"urgent turn", "collect", "/urgent hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{mode: tc.mode})
			turns := h.session(parityKey, true)
			ws := h.ws()
			ws.send("w1", tc.text)
			if s := ws.ack(t, "w1"); s != "accepted" {
				t.Fatalf("detached send ack = %q, want accepted", s)
			}
			turns.turn(t, "detached turn", parityOutcome{Panic: "parity: detached turn panic"})
			h.waitEngineIdle()
			onlyErrorAck(t, ws.errorAcks(), "w1", parityPanicReply)
			ws.send("w2", "after")
			turns.turn(t, "the next turn", okTurn("R"))
			h.waitEngineIdle()
		})
	}
}

// keyIsFree reports whether parityKey has no owner: a probe request takes the
// owner slot (released again) instead of queueing behind one. In passthrough
// mode every request is detached and no owner exists to leak, so it is free.
func (h *parityHarness) keyIsFree(t *testing.T) bool {
	t.Helper()
	turns := h.hs.wiring.turns
	ack := turns.Submit(context.Background(), turn.Request{Key: parityKey, Text: "probe"}, neverRunAdmission{})
	if ack == turn.AckOwner {
		turns.Retire(context.Background(), parityKey)
	}
	return ack == turn.AckOwner || ack == turn.AckDetached
}

// rebuiltEngine builds a second send engine over h's router and broadcaster,
// its turns on a fresh Orchestrator, with h's queue options, with sender in
// front.
func (h *parityHarness) rebuiltEngine(t *testing.T, sender turn.Sender) *sendEngine {
	t.Helper()
	w := h.hs.wiring
	e := newSendEngine(sendEngineOpts{
		Turns:    turn.New(w.queue, sender),
		Ctx:      h.srv.appCtx,
		Router:   h.router,
		Resolver: w.resolver,
		Agents:   w.agents,
		Notify:   w.bcast,
	})
	t.Cleanup(e.drain)
	return e
}

// finishLog logs each Finish its origin's deliveries get, in front of them.
type finishLog struct {
	turn.Origin
	log *orderLog
}

func (o finishLog) Begin(ctx context.Context, t turn.TurnInfo) turn.Delivery {
	d := o.Origin.Begin(ctx, t)
	if d == nil {
		return nil
	}
	return finishLogDelivery{d, o.log}
}

type finishLogDelivery struct {
	turn.Delivery
	log *orderLog
}

func (d finishLogDelivery) Finish(ctx context.Context, out turn.Outcome) {
	d.log.add(fmt.Sprintf("finish panic=%v", out.Panic))
	d.Delivery.Finish(ctx, out)
}

// Row 10 flipped in D: NotifyIdle runs after the panic has been delivered.
func TestTurnParity10_Dash_NotifyIdleAfterPanicHandling(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	log := &orderLog{}
	e := h.rebuiltEngine(t, idleOrderSender{h.turnSender(), log})
	ws := h.ws()
	if _, status, err := e.sessionSend(sendParams{Key: parityKey, Text: "first"}, finishLog{e.wsOrigin(ws.c, "w1", parityKey), log}); err != nil || status != sendAckAccepted {
		t.Fatalf("sessionSend = %q, %v", status, err)
	}
	turns.turn(t, "owner turn", parityOutcome{Panic: "parity: owner turn panic"})
	waitEngineIdle(t, e)
	want := []string{"finish panic=true", "idle"}
	if got := log.snapshot(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("dashboard owner-loop panic order = %q, want %q (NotifyIdle runs after the recover)", got, want)
	}
	onlyErrorAck(t, ws.errorAcks(), "w1", parityPanicReply)
}

// Row 11 flipped in D: a dashboard turn's panic is counted.
func TestTurnParity11_Dash_PanicIsCounted(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	before := metrics.PanicRecoveredTotal.Value()
	ws.send("w1", "first")
	turns.turn(t, "owner turn", parityOutcome{Panic: "parity: owner turn panic"})
	ws.waitFor(t, "panic ack", func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
	h.waitEngineIdle()
	if got := metrics.PanicRecoveredTotal.Value() - before; got != 1 {
		t.Fatalf("PanicRecoveredTotal moved by %d on a dashboard owner-loop panic, want 1", got)
	}
}

// Row 12 flipped in D: a drain turn's panic reaches the send it carried and
// the send it discarded, never the owner's already-finished first send.
func TestTurnParity12_Dash_DrainPanicReportedToBatch(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	ws.send("w2", "second")
	ws.ack(t, "w2")
	turns.answer(okTurn("R1"))
	turns.next(t, "drain turn")
	ws.send("w3", "third")
	ws.ack(t, "w3")
	turns.answer(parityOutcome{Panic: "parity: drain turn panic"})
	h.waitEngineIdle()
	turns.noMoreTurns(t)
	acks := ws.errorAcks()
	ids := make([]string, 0, len(acks))
	for _, a := range acks {
		if a.Error != parityPanicReply {
			t.Errorf("error ack %+v, want %q", a, parityPanicReply)
		}
		ids = append(ids, a.ID)
	}
	if slices.Sort(ids); strings.Join(ids, ",") != "w2,w3" {
		t.Fatalf("error acks after a drain-turn panic went to %q, want w2 (in the turn) and w3 (discarded), not w1", ids)
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
	if !h.keyIsFree(t) {
		t.Fatal("dashboard owner exiting on ctx cancel left the key owned")
	}
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("discarded dashboard message got error acks %+v, want none", acks)
	}
}

// Row 14 flipped in D: a WS send pushed out of a full queue is told. An HTTP
// one is not: it has no per-request channel, and a send_error would reach
// every tab on the key.
func TestTurnParity14_Dash_EvictedSendIsTold(t *testing.T) {
	t.Run("ws", func(t *testing.T) {
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
		onlyErrorAck(t, ws.errorAcks(), "w2", evictedSendMsg)
	})
	t.Run("http is silent", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{maxDepth: 1})
		turns := h.session(parityKey, false)
		ws := h.ws()
		h.httpSend(t, "first")
		turns.next(t, "owner turn")
		for _, text := range []string{"second", "third"} {
			if s := h.httpSend(t, text); s != "queued" {
				t.Fatalf("HTTP %s status = %q", text, s)
			}
		}
		turns.answer(okTurn("R1"))
		if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "third" {
			t.Fatalf("drain turn = %q, want only the surviving message", c.Text)
		}
		h.waitEngineIdle()
		if errs := ws.framesOfType("send_error"); len(errs) != 0 {
			t.Fatalf("evicted HTTP send: send_error frames %+v, want none", errs)
		}
	})
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

func TestTurnParity17_Dash_NoTakeoverNoNewSessionNotice(t *testing.T) {
	h := newParityHarness(t, parityOpts{interim: true})
	turns := h.session(parityKey, false)
	e := h.rebuiltEngine(t, newSessionSender{h.turnSender()})
	ws := h.ws()
	if _, _, err := e.sessionSend(sendParams{Key: parityKey, Text: "first"}, e.wsOrigin(ws.c, "w1", parityKey)); err != nil {
		t.Fatal(err)
	}
	turns.turn(t, "owner turn on a fresh session", okTurn("R1"))
	waitEngineIdle(t, e)
	if r, acks := h.plat.allReplies(), ws.errorAcks(); len(r) != 0 || len(acks) != 0 {
		t.Fatalf("dashboard first turn on a new session: IM replies %q, error acks %+v; want neither", r, acks)
	}
}

func TestTurnParity18_Dash_MergeFollowerIsSilent(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "passthrough"})
	turns := h.session(parityKey, true)
	ws := h.ws()
	ws.send("w1", "follower")
	turns.turn(t, "follower turn", parityOutcome{Result: &clievent.SendResult{MergedCount: 2, MergedWithHead: 1}})
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
			if !h.keyIsFree(t) {
				t.Fatal("a send refused during shutdown left the key owned")
			}
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

// Row 27: the autosave moved into turnSender (D), the Sender every entry's
// turns go through; through the production wiring, a failed turn on a cron
// key leaves the job's empty prompt alone and a successful one fills it.
func TestTurnParity27_Dash_CronPromptAutosave(t *testing.T) {
	h := newParityHarness(t, parityOpts{cron: true})
	job := &cron.Job{Schedule: "@every 1h", Platform: "dashboard", ChatID: "c1", ChatType: "direct", Paused: true}
	if err := h.sched.AddJob(job); err != nil {
		t.Fatal(err)
	}
	prompt := func() string {
		j, _ := h.sched.GetJob(job.ID)
		return j.Prompt
	}
	cronKey := sessionkey.CronKey(job.ID)
	turns := h.session(cronKey, false)
	ws := h.ws()
	ws.sendTo(cronKey, "w1", "do Z")
	turns.turn(t, "failed cron turn", parityOutcome{Err: errParityBoom})
	h.waitEngineIdle()
	if p := prompt(); p != "" {
		t.Fatalf("job prompt after a failed turn = %q, want it left empty", p)
	}
	ws.sendTo(cronKey, "w2", "do X")
	turns.turn(t, "successful cron turn", okTurn("R"))
	h.waitEngineIdle()
	if p := prompt(); p != "do X" {
		t.Fatalf("job prompt after a successful turn = %q, want %q", p, "do X")
	}
}

// Row 28: a busy dashboard send takes the queue path and waits behind the
// owner; no path around the queue exists.
func TestTurnParity28_Dash_QueuePathNotLegacy(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "owner turn")
	if s := h.httpSend(t, "second"); s != "queued" {
		t.Fatalf("HTTP send while busy = %q, want queued", s)
	}
	turns.answer(okTurn("R1"))
	turns.turn(t, "drain turn", okTurn("R2"))
	h.waitEngineIdle()
}
