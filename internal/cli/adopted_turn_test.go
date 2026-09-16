package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// armReconnectMidTurn mirrors what SpawnReconnect does for a backlog whose last
// event is not a result: State Running + the one-shot flag + the latch, all
// before startReadLoop. Tests use it instead of driving a real shim reconnect so
// the assertions stay on the latch and not on the reconnect plumbing; the
// ordering itself is pinned by TestReconnectMidTurn_ResultTransitionsToReady.
func armReconnectMidTurn(p *Process) {
	p.mu.Lock()
	p.state = StateRunning
	p.mu.Unlock()
	p.reconnectedMidTurn.Store(true)
	p.adopted.arm()
}

// TestApplyReconnectVerdict_ArmsOnlyWhatTheBacklogJustifies covers the wiring
// from verdict to latch, including the case with no observable side effect: a
// backlog with nothing in flight must leave the latch unarmed, so the difference
// between "no turn to adopt" and "a turn that answered nothing" survives.
//
// Every AdoptedOutcome call here passes a deadline rather than context.Background:
// no read loop is running to resolve a latch, so an implementation that arms one
// it should not have would hang the test instead of failing it — which is a worse
// signal, and hides the mutation this test exists to catch.
func TestApplyReconnectVerdict_ArmsOnlyWhatTheBacklogJustifies(t *testing.T) {
	newCtx := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	t.Run("mid turn arms and waits", func(t *testing.T) {
		p := &Process{}
		p.applyReconnectVerdict(true, nil)
		if !p.reconnectedMidTurn.Load() {
			t.Error("reconnectedMidTurn not armed for a mid-turn backlog")
		}
		if p.State() != StateRunning {
			t.Errorf("State = %v, want StateRunning", p.State())
		}
		if !p.AdoptedTurnPending() {
			t.Error("latch not pending for a mid-turn backlog")
		}
	})

	t.Run("finished turn latches the replayed result", func(t *testing.T) {
		p := &Process{}
		p.applyReconnectVerdict(false, &clievent.Event{
			Type: "result", SubType: "success", Result: "from the backlog", SessionID: "s1",
		})
		if p.reconnectedMidTurn.Load() {
			t.Error("reconnectedMidTurn armed for a turn that already ended")
		}
		if p.AdoptedTurnPending() {
			t.Error("latch still pending; the replayed result was not stored")
		}
		out, err := p.AdoptedOutcome(newCtx(t))
		if err != nil {
			t.Fatalf("AdoptedOutcome: %v", err)
		}
		if out.End != AdoptedEndResult || out.Result.Text != "from the backlog" {
			t.Errorf("outcome = %+v, want the replayed result", out)
		}
	})

	t.Run("nothing in flight leaves the latch unarmed", func(t *testing.T) {
		p := &Process{}
		p.applyReconnectVerdict(false, nil)
		if p.reconnectedMidTurn.Load() {
			t.Error("reconnectedMidTurn armed with nothing in flight")
		}
		if _, err := p.AdoptedOutcome(newCtx(t)); !errors.Is(err, ErrNoAdoptableTurn) {
			t.Errorf("err = %v, want ErrNoAdoptableTurn", err)
		}
	})
}

// TestAdoptedTurn_LatchKeepsTheResultTextTheEventLogDrops is the defect this
// latch exists for: a result arriving with no Send active leaves its text
// nowhere. The test asserts both halves — the latch has the text, and the
// durable event log still does not — so a future change that starts persisting
// result text does not leave this looking like a redundant copy.
func TestAdoptedTurn_LatchKeepsTheResultTextTheEventLogDrops(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	defer p.Kill()

	armReconnectMidTurn(p)
	p.startReadLoop()

	srv.SendStdout(`{"type":"result","subtype":"success","result":"the answer","session_id":"s1","total_cost_usd":0.25}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("AdoptedOutcome: %v", err)
	}
	if out.End != AdoptedEndResult {
		t.Errorf("End = %q, want %q", out.End, AdoptedEndResult)
	}
	if out.Result.Text != "the answer" {
		t.Errorf("Text = %q, want %q — the late result's text was not kept", out.Result.Text, "the answer")
	}
	if out.SubType != "success" {
		t.Errorf("SubType = %q, want success", out.SubType)
	}
	if out.Result.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", out.Result.SessionID)
	}
	if out.Result.CostUSD != 0.25 {
		t.Errorf("CostUSD = %v, want 0.25", out.Result.CostUSD)
	}

	// The other half: the event log recorded the turn boundary without the text.
	// This is why the latch is not redundant.
	var sawResult bool
	for _, e := range p.eventLog.EntriesSince(0) {
		if e.Type != "result" {
			continue
		}
		sawResult = true
		if e.Detail != "" || e.Summary != "" {
			t.Errorf("event log result entry carries text (Detail=%q Summary=%q); "+
				"if result text is now durable, re-justify the latch", e.Detail, e.Summary)
		}
	}
	if !sawResult {
		t.Error("no result entry in the event log; the fixture did not reach readLoop")
	}
}

// TestAdoptedTurn_ReadableTwice pins latch, not queue: an adoption that reads the
// outcome and then crashes before recording it must find the same answer on the
// next read, and a second reader must not block.
func TestAdoptedTurn_ReadableTwice(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	defer p.Kill()

	armReconnectMidTurn(p)
	p.startReadLoop()
	srv.SendStdout(`{"type":"result","subtype":"success","result":"twice","session_id":"s1"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("first AdoptedOutcome: %v", err)
	}
	second, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("second AdoptedOutcome: %v", err)
	}
	if first.End != second.End || first.SubType != second.SubType ||
		first.Result.Text != second.Result.Text || first.Result.CostUSD != second.Result.CostUSD {
		t.Errorf("second read differs: %+v vs %+v", first, second)
	}
}

// TestAdoptedTurn_CLIExitDoesNotOverwriteAResultAlreadyLatched: every process
// eventually dies, so resolveExit runs after every successful adoption too. If it
// could overwrite, a run that finished with an answer would be re-read as
// cli_exited and recorded as interrupted — the exact failure the latch exists to
// prevent, arriving later instead of sooner.
func TestAdoptedTurn_CLIExitDoesNotOverwriteAResultAlreadyLatched(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)

	armReconnectMidTurn(p)
	p.startReadLoop()
	srv.SendStdout(`{"type":"result","subtype":"success","result":"answered","session_id":"s1"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := p.AdoptedOutcome(ctx); err != nil {
		t.Fatalf("AdoptedOutcome: %v", err)
	}

	// Now lose the CLI, which drives readLoop's unwind and therefore resolveExit.
	srv.Close()
	testhelper.Eventually(t, func() bool {
		return p.State() == StateDead
	}, 5*time.Second, "process did not reach StateDead after the shim went away")

	out, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("AdoptedOutcome after exit: %v", err)
	}
	if out.End != AdoptedEndResult || out.Result.Text != "answered" {
		t.Errorf("outcome = %+v after CLI exit; want the latched result to survive — "+
			"a succeeded run must not become cli_exited just because the process later died", out)
	}
}

// TestAdoptedTurn_NotArmedFailsFast covers the process that was never a mid-turn
// reconnect. It must say so immediately: a caller reconciling a stale marker
// would otherwise sit out its whole budget per marker before recording the
// interrupted run it could have recorded at once.
func TestAdoptedTurn_NotArmedFailsFast(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	defer p.Kill()
	p.startReadLoop()

	if p.AdoptedTurnPending() {
		t.Error("AdoptedTurnPending on a process that never reconnected mid-turn")
	}

	// A context that is already generous: if the implementation waits at all,
	// the elapsed time exposes it rather than the error being masked by a
	// deadline that happened to be short.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := p.AdoptedOutcome(ctx)
	if !errors.Is(err, ErrNoAdoptableTurn) {
		t.Fatalf("err = %v, want ErrNoAdoptableTurn", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("waited %v before reporting ErrNoAdoptableTurn; must not block", elapsed)
	}
}

// TestAdoptedTurn_CLIExitResolvesInsteadOfHanging: the CLI dying without a
// result is knowable at once, so a waiter must be woken with cli_exited rather
// than left to time out. A caller distinguishing success from interrupted needs
// End, not an empty Text it would have to guess about.
func TestAdoptedTurn_CLIExitResolvesInsteadOfHanging(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)

	armReconnectMidTurn(p)
	p.startReadLoop()

	if !p.AdoptedTurnPending() {
		t.Fatal("AdoptedTurnPending false right after arming")
	}

	// No result: the shim goes away mid-turn.
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("AdoptedOutcome after CLI exit: %v — a waiter must not be left to its deadline", err)
	}
	if out.End != AdoptedEndCLIExited {
		t.Errorf("End = %q, want %q", out.End, AdoptedEndCLIExited)
	}
	if out.Result.Text != "" {
		t.Errorf("Text = %q, want empty — no result was produced", out.Result.Text)
	}
	if p.AdoptedTurnPending() {
		t.Error("still pending after the outcome was latched")
	}
}

// TestAdoptedTurn_LiveSendKeepsItsOwnResult pins the mutual exclusion on the
// passthrough path: with a slot owning the turn, the fan-out settles the result
// and the latch must stay empty. Without this, an adoption running against a
// session a dashboard user is actively using would consume that user's reply.
func TestAdoptedTurn_LiveSendKeepsItsOwnResult(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()

	// Arm before the read loop, as SpawnReconnect does, then let a live slot
	// claim the turn — the state a dashboard user's message lands in while an
	// adoption is still outstanding.
	armReconnectMidTurn(sh.proc)
	go sh.proc.readLoop()

	resultCh := make(chan *clievent.SendResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "hello", nil, nil, "")
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()

	input := sh.expectWrite(t, 2*time.Second)
	sh.emitInit("s1")
	sh.emitReplay(input.UUID, "hello")
	sh.emitResult("s1", "mine")

	select {
	case res := <-resultCh:
		if res.Text != "mine" {
			t.Errorf("Send got Text = %q, want mine", res.Text)
		}
	case err := <-errCh:
		t.Fatalf("SendPassthrough: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("SendPassthrough did not return; the latch may have swallowed its result")
	}

	// The latch must be untouched: this result had an owner.
	if !sh.proc.AdoptedTurnPending() {
		out, _ := sh.proc.AdoptedOutcome(context.Background())
		t.Errorf("latch resolved with %+v; a result claimed by a live Send must not be adopted", out)
	}
}

// TestReconnectVerdict_HandsBackTheFinishedResult covers the third arming case:
// the turn ended while naozhi was down, so its result is in the drained backlog
// and this is the only moment it exists in memory. The verdict must hand it over
// instead of only reporting "not mid-turn".
func TestReconnectVerdict_HandsBackTheFinishedResult(t *testing.T) {
	proto := &ClaudeProtocol{}
	replays := []shim.ServerMsg{
		{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"s1"}`},
		{Type: "replay", Line: `{"type":"result","subtype":"success","result":"finished while down","session_id":"s1"}`},
	}
	midTurn, finished := reconnectVerdict(replays, proto)
	if midTurn {
		t.Error("midTurn = true for a backlog ending in a result")
	}
	if finished == nil {
		t.Fatal("finished = nil; the result frame the walk already had was dropped")
	}
	if finished.Result != "finished while down" {
		t.Errorf("Result = %q, want %q", finished.Result, "finished while down")
	}
	if finished.SubType != "success" {
		t.Errorf("SubType = %q, want success", finished.SubType)
	}
}

// TestReconnectVerdict_MidTurnAndFinishedAreExclusive: the two returns describe
// one classification, and a caller switching on them must not need a tie-break.
func TestReconnectVerdict_MidTurnAndFinishedAreExclusive(t *testing.T) {
	proto := &ClaudeProtocol{}
	cases := []struct {
		name    string
		replays []shim.ServerMsg
	}{
		{"mid turn", []shim.ServerMsg{
			{Type: "replay", Line: `{"type":"result","result":"old","session_id":"s1"}`},
			{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"still going"}]},"session_id":"s1"}`},
		}},
		{"finished", []shim.ServerMsg{
			{Type: "replay", Line: `{"type":"result","result":"done","session_id":"s1"}`},
		}},
		{"nothing semantic", []shim.ServerMsg{
			{Type: "replay", Line: `{"type":"system","subtype":"init","session_id":"s1"}`},
		}},
		{"empty backlog", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			midTurn, finished := reconnectVerdict(tc.replays, proto)
			if midTurn && finished != nil {
				t.Errorf("both set: midTurn=%v finished=%+v", midTurn, finished)
			}
		})
	}
}
