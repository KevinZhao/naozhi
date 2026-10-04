package server

// The relay entry (#3032): a send a primary relays over the reverse link runs
// on the same turn.Orchestrator as IM and the dashboard. These drive
// Server.SubmitRelayed, the upstream connector's TurnSubmitter, on the parity
// harness.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

func (h *parityHarness) relay(t *testing.T, text string) string {
	t.Helper()
	status, err := h.srv.SubmitRelayed(context.Background(), parityKey, text, "")
	if err != nil {
		t.Fatalf("relayed %q: %v", text, err)
	}
	return status
}

// systemEvents returns the summaries of key's system events.
func (h *parityHarness) systemEvents(key string) []string {
	sess := h.router.SessionFor(key)
	if sess == nil {
		return nil
	}
	var out []string
	for _, e := range sess.EventEntries() {
		if e.Type == clievent.KindSystem {
			out = append(out, e.Summary)
		}
	}
	return out
}

func TestRelay_BareResetResetsAndSendsNothing(t *testing.T) {
	for _, text := range []string{"/new", "/New ", " /CLEAR"} {
		t.Run(text, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{})
			turns := h.session(parityKey, false)
			h.router.SetWorkspace(parityChatKey, t.TempDir())
			if s := h.relay(t, text); s != "reset" {
				t.Fatalf("status = %q, want reset", s)
			}
			if got := h.router.Workspace(parityChatKey); got != "" {
				t.Fatalf("workspace override after relayed %q = %q, want it discarded by the reset", text, got)
			}
			turns.noMoreTurns(t)
		})
	}
}

func TestRelay_ResetWithArgumentIsText(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	if s := h.relay(t, "/new foo"); s != "accepted" {
		t.Fatalf("status = %q, want accepted", s)
	}
	if c := turns.turn(t, "turn", okTurn("R")); c.Text != "/new foo" {
		t.Fatalf("turn text = %q, want the literal command", c.Text)
	}
	h.waitEngineIdle()
	if got := h.systemEvents(parityKey); len(got) != 0 {
		t.Fatalf("system events after a turn that succeeded = %q, want none", got)
	}
}

// Sends relayed while a turn runs are queued and merged into one turn.
func TestRelay_SendsOnABusyKeyCoalesce(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	if s := h.relay(t, "first"); s != "accepted" {
		t.Fatalf("first status = %q, want accepted", s)
	}
	turns.next(t, "owner turn")
	for _, text := range []string{"second", "third"} {
		if s := h.relay(t, text); s != "queued" {
			t.Fatalf("%s status = %q, want queued", text, s)
		}
	}
	turns.answer(okTurn("R1"))
	c := turns.turn(t, "merged turn", okTurn("R2"))
	if !strings.Contains(c.Text, "second") || !strings.Contains(c.Text, "third") {
		t.Fatalf("drain turn text = %q, want both queued sends merged", c.Text)
	}
	h.waitEngineIdle()
	turns.noMoreTurns(t)
}

// An IM owner loop drains a relayed send queued behind it: one queue per key.
func TestRelay_SharesTheQueueWithIM(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	owner := h.imAsync("m1", "from im")
	turns.next(t, "IM owner turn")
	if s := h.relay(t, "from relay"); s != "queued" {
		t.Fatalf("status = %q, want queued behind the IM turn", s)
	}
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "from relay" {
		t.Fatalf("drain turn text = %q, want the relayed send", c.Text)
	}
	h.waitDone(owner, "IM owner loop")
	turns.noMoreTurns(t)
}

func TestRelay_UrgentPreempts(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, true)
	if s := h.relay(t, "/urgent hi"); s != "accepted" {
		t.Fatalf("status = %q, want accepted", s)
	}
	if c := turns.turn(t, "urgent turn", okTurn("R")); c.Text != "hi" || c.Priority != "now" {
		t.Fatalf("turn = %+v, want text hi at priority now", c)
	}
	h.waitEngineIdle()
	if _, err := h.srv.SubmitRelayed(context.Background(), parityKey, "/urgent", ""); !errors.Is(err, errUrgentUsage) {
		t.Fatalf("bare /urgent: err = %v, want the usage error", err)
	}
	turns.noMoreTurns(t)
}

// A failed turn leaves a sanitized system event in the session's EventLog,
// which is how the primary's tabs hear about it.
func TestRelay_TurnFailureIsASystemEvent(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.relay(t, "hi")
	turns.turn(t, "turn", parityOutcome{Err: errParityBoom})
	h.waitEngineIdle()
	want := relayFailPrefix + asyncErrorMessage(errParityBoom)
	if got := h.systemEvents(parityKey); len(got) != 1 || got[0] != want {
		t.Fatalf("system events = %q, want [%q]", got, want)
	}
}

func TestRelay_TurnPanicIsASystemEvent(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.relay(t, "hi")
	turns.turn(t, "turn", parityOutcome{Panic: "boom"})
	h.waitEngineIdle()
	if got := h.systemEvents(parityKey); len(got) != 1 || got[0] != relayFailPrefix+turnPanicMsg {
		t.Fatalf("system events = %q, want the panic message", got)
	}
}

// A turn the node's shutdown cuts short is not reported: the shim may carry
// it through the restart.
func TestRelay_TurnCutShortByShutdownIsSilent(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.relay(t, "hi")
	turns.next(t, "turn")
	drained := make(chan struct{})
	go func() {
		h.engine().drain()
		close(drained)
	}()
	<-h.engine().ctx.Done()
	turns.answer(parityOutcome{Err: context.Canceled})
	h.waitDone(drained, "engine drain")
	if got := h.systemEvents(parityKey); len(got) != 0 {
		t.Fatalf("system events = %q, want none", got)
	}
}

// A relayed owner observing a drain turn of other entries' messages does not
// report that turn's failure: those entries answer for their own.
func TestRelay_OwnerObservingAnotherEntrysTurnIsSilent(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.relay(t, "first")
	turns.next(t, "owner turn")
	h.imSend("m1", "from im")
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", parityOutcome{Err: errParityBoom}); c.Text != "from im" {
		t.Fatalf("drain turn text = %q, want the IM message", c.Text)
	}
	h.waitEngineIdle()
	for _, e := range h.systemEvents(parityKey) {
		if strings.HasPrefix(e, relayFailPrefix) {
			t.Fatalf("system events = %q, want no relay failure for a turn it only observed", h.systemEvents(parityKey))
		}
	}
}

// An outcome the user already knows about (their reset, their /urgent) is not
// reported.
func TestRelay_InformationalFailureIsSilent(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.relay(t, "hi")
	turns.turn(t, "turn", parityOutcome{Err: clierr.ErrSessionReset})
	h.waitEngineIdle()
	if got := h.systemEvents(parityKey); len(got) != 0 {
		t.Fatalf("system events = %q, want none", got)
	}
}

func TestRelay_EvictedSendIsASystemEvent(t *testing.T) {
	h := newParityHarness(t, parityOpts{maxDepth: 1})
	turns := h.session(parityKey, false)
	h.relay(t, "first")
	turns.next(t, "owner turn")
	h.relay(t, "a")
	h.relay(t, "b") // pushes "a" out of the one-slot queue
	if got := h.systemEvents(parityKey); len(got) != 1 || got[0] != relayFailPrefix+evictedSendMsg {
		t.Fatalf("system events = %q, want the eviction message", got)
	}
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "b" {
		t.Fatalf("drain turn text = %q, want b", c.Text)
	}
	h.waitEngineIdle()
}

func TestRelay_BusyWithTheQueueDisabledIsAnError(t *testing.T) {
	h := newParityHarness(t, parityOpts{maxDepth: -1})
	turns := h.session(parityKey, false)
	h.relay(t, "first")
	turns.next(t, "owner turn")
	if _, err := h.srv.SubmitRelayed(context.Background(), parityKey, "second", ""); !errors.Is(err, errSendBusy) {
		t.Fatalf("err = %v, want errSendBusy", err)
	}
	turns.answer(okTurn("R1"))
	h.waitEngineIdle()
	turns.noMoreTurns(t)
}

func TestRelay_ShuttingDownIsAnError(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	h.engine().drain()
	if _, err := h.srv.SubmitRelayed(context.Background(), parityKey, "hi", ""); !errors.Is(err, errRelayShuttingDown) {
		t.Fatalf("err = %v, want errRelayShuttingDown", err)
	}
	turns.noMoreTurns(t)
}

// The session is created before SubmitRelayed answers, with the key's agent
// options: a spawn the router rejects is the caller's error and runs no turn.
func TestRelay_SpawnFailureIsTheError(t *testing.T) {
	t.Run("no session can be spawned", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		_, err := h.srv.SubmitRelayed(context.Background(), parityKey, "hi", "")
		if err == nil || !strings.Contains(err.Error(), "get session") {
			t.Fatalf("err = %v, want the spawn failure", err)
		}
		h.waitEngineIdle()
	})
	t.Run("agent options apply", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{agents: map[string]session.AgentOpts{"general": {Model: "not a model"}}})
		turns := h.session(parityKey, false)
		_, err := h.srv.SubmitRelayed(context.Background(), parityKey, "hi", "")
		if err == nil || !strings.Contains(err.Error(), "get session") {
			t.Fatalf("err = %v, want GetOrCreate to reject the agent's model", err)
		}
		h.waitEngineIdle()
		turns.noMoreTurns(t)
	})
}

// ctxRecordingRouter records the ctx GetOrCreate is called with.
type ctxRecordingRouter struct {
	*session.Router
	got chan context.Context
}

func (r ctxRecordingRouter) GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
	r.got <- ctx
	return r.Router.GetOrCreate(ctx, key, opts)
}

type relayCtxKey struct{}

// The preflight spawn runs on the caller's ctx, so a dropped link abandons a
// spawn nobody will hear about.
func TestRelay_PreflightRunsOnTheCallersCtx(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	w := h.hs.wiring
	rec := ctxRecordingRouter{Router: h.router, got: make(chan context.Context, 4)}
	e := newSendEngine(sendEngineOpts{
		Turns:    turn.New(w.queue, h.turnSender()),
		Ctx:      h.srv.appCtx,
		Router:   rec,
		Resolver: w.resolver,
		Agents:   w.agents,
		Notify:   w.bcast,
	})
	t.Cleanup(e.drain)
	ctx := context.WithValue(context.Background(), relayCtxKey{}, "conn")
	if _, err := e.relaySend(ctx, parityKey, "hi", ""); err != nil {
		t.Fatal(err)
	}
	preflight := <-rec.got
	turns.turn(t, "turn", okTurn("R"))
	waitEngineIdle(t, e)
	if preflight.Value(relayCtxKey{}) != "conn" {
		t.Fatal("the preflight GetOrCreate did not run on the caller's ctx")
	}
}

// A relayed workspace is the chat's override, as a dashboard send's is.
func TestRelay_WorkspaceIsTheChatOverride(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	turns := h.session(parityKey, false)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.srv.SubmitRelayed(context.Background(), parityKey, "hi", dir); err != nil {
		t.Fatal(err)
	}
	turns.turn(t, "turn", okTurn("R"))
	h.waitEngineIdle()
	if got := h.router.Workspace(parityChatKey); got != dir {
		t.Fatalf("chat workspace = %q, want %q", got, dir)
	}
}

// R172-SEC-M4: the system event is persisted and broadcast, so what it
// carries goes through SanitizeForLog.
func TestRelayOrigin_ReportIsSanitized(t *testing.T) {
	h := newParityHarness(t, parityOpts{})
	h.session(parityKey, false)
	o := &relayOrigin{dashOrigin: dashOrigin{key: parityKey}, sessions: h.router}
	o.report("bad\u202eline\u0085two\nthree")
	got := h.systemEvents(parityKey)
	if len(got) != 1 || !strings.HasPrefix(got[0], relayFailPrefix) {
		t.Fatalf("system events = %q, want one 发送失败 event", got)
	}
	for _, r := range []string{"\u202e", "\u0085", "\n"} {
		if strings.Contains(got[0], r) {
			t.Errorf("system event %q carries %q", got[0], r)
		}
	}
}
