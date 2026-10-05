package cli

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// ---------------------------------------------------------------------------
// #1779: drainStaleEvents re-enqueue must not panic when readLoop has closed
// eventCh between the isChanAlive(done) guard and the actual send.
// ---------------------------------------------------------------------------

// TestSafeReenqueue_SendOnClosedDoesNotPanic exercises safeReenqueue directly
// against an already-closed eventCh. readLoop closes done strictly before
// eventCh, but a producer that passed the isChanAlive(done) guard can still
// race the eventCh close. A bare `select { case ch<-ev: default: }` panics on
// a closed channel (the send case is always ready-to-run, so select picks it).
// safeReenqueue must recover and drop silently.
func TestSafeReenqueue_SendOnClosedDoesNotPanic(t *testing.T) {
	t.Parallel()

	p := &Process{
		eventCh: make(chan clievent.Event, 1),
		done:    make(chan struct{}),
	}
	close(p.eventCh)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("safeReenqueue panicked on closed eventCh: %v", r)
		}
	}()

	p.safeReenqueue(clievent.Event{Type: "result", SessionID: "race"})
}

// TestDrainStaleEvents_CtxDoneReenqueueOnClosedChan pins #1779 at the
// drainStaleEvents level via the ctx.Done re-enqueue arm. Setup: the previous
// turn was interrupted-while-running, so drain enters the settle window; the
// channel is closed there (CLI exited) which short-circuits to `drain`. We
// then drive the ctx.Done re-enqueue with done still OPEN (isChanAlive lets the
// re-enqueue proceed) but eventCh CLOSED — the exact race the recover guards.
// Without safeReenqueue's recover this panics; with it the drain returns the
// ctx error cleanly.
func TestDrainStaleEvents_CtxDoneReenqueueOnClosedChan(t *testing.T) {
	t.Parallel()

	p := &Process{
		eventCh: make(chan clievent.Event, 4),
		done:    make(chan struct{}), // OPEN: isChanAlive reports alive
	}
	p.turn.interrupted.Store(true)
	p.turn.interruptedRun.Store(true)

	// One post-cutoff event so holdback is non-empty when we reach the
	// re-enqueue, then close the channel so the settle-window read observes
	// ok==false and jumps to drain, and any re-enqueue targets a dead channel.
	future := time.Now().Add(time.Hour)
	p.eventCh <- clievent.Event{Type: "assistant", SessionID: "fresh", RecvAt: future}
	close(p.eventCh)

	// Pre-cancel so the drain loop's ctx.Done arm fires and exercises the
	// holdback re-enqueue path onto the now-closed eventCh.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("drainStaleEvents panicked on closed eventCh: %v", r)
		}
	}()

	// Either the ctx error or nil is acceptable here; the assertion under test
	// is "does not panic". The send-on-closed would crash the goroutine.
	_ = p.drainStaleEvents(ctx)
}

// ---------------------------------------------------------------------------
// #1778: SpawnReconnect must arm reconnectedMidTurn + StateRunning BEFORE the
// readLoop starts, so a result that arrives immediately after reconnect does
// not get processed by the stray-result handler while the flag is still unset
// (which would strand the session in StateRunning).
// ---------------------------------------------------------------------------

// TestReconnectMidTurn_ResultTransitionsToReady simulates the post-reconnect
// invariant the #1778 ordering fix protects: once reconnectedMidTurn is armed
// and state is Running, a `result` event delivered by readLoop transitions the
// state back to Ready (no Send() ever runs). With the arm happening before
// startReadLoop, even a result arriving in the first read iteration is handled.
func TestReconnectMidTurn_ResultTransitionsToReady(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	defer p.Kill()

	// Mirror SpawnReconnect's ordering: arm BEFORE the read loop starts.
	p.turn.mu.Lock()
	p.turn.state = StateRunning
	p.turn.mu.Unlock()
	p.turn.reconnectedMidTurn.Store(true)

	done := make(chan struct{}, 1)
	p.SetOnTurnDone(func() {
		select {
		case done <- struct{}{}:
		default:
		}
	})

	p.startReadLoop()

	// Result arrives with no active Send(): the stray-result handler must
	// consume the armed flag and flip Running → Ready.
	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onTurnDone not called: armed reconnect result did not settle the turn")
	}

	// Allow the readLoop state transition to be observed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.State() == StateReady {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("State = %v after reconnect result, want StateReady (session stuck Running)", p.State())
}

// TestIsMidTurn covers the helper SpawnReconnect uses to decide whether to arm
// reconnectedMidTurn. The arm decision depends ONLY on the already-drained
// replays, which is why #1778 can safely evaluate it before startReadLoop.
func TestIsMidTurn(t *testing.T) {
	proto := &ClaudeProtocol{}

	tests := []struct {
		name    string
		replays []shim.ServerMsg
		want    bool
	}{
		{
			name:    "no replays is not mid-turn",
			replays: nil,
			want:    false,
		},
		{
			name: "last event is result -> turn complete",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"session_id":"s1"}`},
				{Type: "replay", Line: `{"type":"result","result":"ok","session_id":"s1"}`},
			},
			want: false,
		},
		{
			name: "last event is assistant -> mid turn",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"result","result":"ok","session_id":"s1"}`},
				{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"still going"}]},"session_id":"s1"}`},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _, _ := reconnectVerdict(tt.replays, 0, proto).settle(nil, ""); got != tt.want {
				t.Errorf("reconnectVerdict() = %v, want %v", got, tt.want)
			}
		})
	}
}

// doneIgnoringProtocol embeds *ClaudeProtocol (inheriting the full Protocol
// surface) and overrides only ReadEvent to return done=true alongside a NON
// result event. It pins the R202606f-ARCH-5 (#2303) contract: turn-end is
// driven by a result clievent.Event, never by the advisory `done` bool. Clone returns
// the same wrapped behaviour so reconnectVerdict's call path is exercised faithfully.
type doneIgnoringProtocol struct{ *ClaudeProtocol }

func (d doneIgnoringProtocol) ReadEvent(line string) ([]clievent.Event, bool, error) {
	// Always claim the turn is done, but only ever emit an assistant event.
	// A caller that honoured `done` would treat this as turn-complete; the
	// documented contract says it must NOT, because there is no result clievent.Event.
	return []clievent.Event{{Type: "assistant"}}, true, nil
}

func (d doneIgnoringProtocol) Clone() Protocol { return d }

// TestIsMidTurn_IgnoresAdvisoryDone verifies reconnectVerdict does not let a
// protocol's done=true short-circuit the result-clievent.Event-based turn-end
// detection. The last (and only) emitted event is an assistant frame, so the
// turn is still in progress regardless of done=true (#2303).
func TestIsMidTurn_IgnoresAdvisoryDone(t *testing.T) {
	proto := doneIgnoringProtocol{ClaudeProtocol: &ClaudeProtocol{}}
	replays := []shim.ServerMsg{
		{Type: "replay", Line: `{"ignored":"the stub ignores the line"}`},
	}
	if got, _, _ := reconnectVerdict(replays, 0, proto).settle(nil, ""); !got {
		t.Errorf("reconnectVerdict = false; want true — done=true must NOT settle a turn that emitted no result clievent.Event (#2303)")
	}
}

// TestIsMidTurn_SkipsControlAck pins the reconnect regression introduced by
// the set_model control channel: a claude control_response carrying a
// request_id parses into a Type:"control_ack" clievent.Event. It is an RPC receipt,
// not turn content, so it must not be the frame that decides mid-turn-ness.
// Scenario: idle claude session → operator switches model (control_response
// buffered in the shim) → naozhi restarts → replay ends with the ack. Reading
// that as "last event != result" arms reconnectedMidTurn, the session sits in
// StateRunning forever (no result will ever come) and every Send fails with
// clierr.ErrProcessBusy.
func TestIsMidTurn_SkipsControlAck(t *testing.T) {
	proto := &ClaudeProtocol{}
	const ack = `{"type":"control_response","response":{"subtype":"success","request_id":"naozhi-setmodel-1"}}`
	const errAck = `{"type":"control_response","response":{"subtype":"error","request_id":"naozhi-setmodel-2","error":"unknown model"}}`

	tests := []struct {
		name    string
		replays []shim.ServerMsg
		want    bool
	}{
		{
			name: "result then set_model ack -> still turn complete",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"session_id":"s1"}`},
				{Type: "replay", Line: `{"type":"result","result":"ok","session_id":"s1"}`},
				{Type: "replay", Line: ack},
			},
			want: false,
		},
		{
			name: "result then rejected set_model ack -> still turn complete",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"result","result":"ok","session_id":"s1"}`},
				{Type: "replay", Line: errAck},
			},
			want: false,
		},
		{
			name: "assistant then set_model ack -> still mid turn",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"still going"}]},"session_id":"s1"}`},
				{Type: "replay", Line: ack},
			},
			want: true,
		},
		{
			name:    "only a set_model ack -> nothing semantic, not mid turn",
			replays: []shim.ServerMsg{{Type: "replay", Line: ack}},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _, _ := reconnectVerdict(tt.replays, 0, proto).settle(nil, ""); got != tt.want {
				t.Errorf("reconnectVerdict() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Frames as CC 2.1.288 writes them (a 3-agent local_workflow capture; ids and
// bulky fields trimmed). The result keeps the CLI's key order, which does not
// start with "type".
const (
	rvInit         = `{"type":"system","subtype":"init","cwd":"/tmp/p","session_id":"s1","tools":["Task","Bash"]}`
	rvAgentToolUse = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Agent","input":{"prompt":"x"}}],"stop_reason":"tool_use"},"session_id":"s1"}`
	rvTaskStarted  = `{"type":"system","subtype":"task_started","task_id":"w113pvmto","tool_use_id":"toolu_1","description":"tiny probe","task_type":"local_workflow","session_id":"s1"}`
	rvProgress     = `{"type":"system","subtype":"task_progress","task_id":"w113pvmto","tool_use_id":"toolu_1","description":"Ask: A","usage":{"total_tokens":10},"uuid":"u1","session_id":"s1"}`
	rvBgChanged    = `{"type":"system","subtype":"background_tasks_changed","tasks":[],"uuid":"u2","session_id":"s1"}`
	rvUpdated      = `{"type":"system","subtype":"task_updated","task_id":"w113pvmto","patch":{"status":"completed","end_time":1791170031523},"uuid":"u3","session_id":"s1"}`
	rvNotification = `{"type":"system","subtype":"task_notification","task_id":"w113pvmto","tool_use_id":"toolu_1","status":"completed","summary":"done","uuid":"u4","session_id":"s1"}`
	rvResult       = `{"duration_api_ms":24185,"stop_reason":"end_turn","session_id":"s1","is_error":false,"subtype":"success","result":"ok","type":"result"}`
)

func rvReplays(firstSeq int64, lines ...string) []shim.ServerMsg {
	out := make([]shim.ServerMsg, len(lines))
	for i, l := range lines {
		out[i] = shim.ServerMsg{Type: "replay", Seq: firstSeq + int64(i), Line: l}
	}
	return out
}

// TestReconnectVerdict_BackgroundTaskFrames: frames a background task emits
// while the session is idle must not make the restart read as mid-turn, while
// a foreground turn they interleave with still does.
func TestReconnectVerdict_BackgroundTaskFrames(t *testing.T) {
	proto := &ClaudeProtocol{}
	tests := []struct {
		name    string
		replays []shim.ServerMsg
		want    verdictKind
	}{
		{"idle with a background workflow running",
			rvReplays(1, rvInit, rvTaskStarted, rvResult, rvProgress, rvProgress, rvBgChanged), verdictFinished},
		{"idle after the workflow ended",
			rvReplays(1, rvTaskStarted, rvResult, rvProgress, rvBgChanged, rvUpdated, rvNotification), verdictFinished},
		{"foreground Agent mid-call under workflow frames",
			rvReplays(1, rvResult, rvInit, rvAgentToolUse, rvProgress, rvProgress), verdictMidTurn},
		{"turn the CLI started off a task_notification",
			rvReplays(1, rvResult, rvProgress, rvNotification, rvInit), verdictMidTurn},
		{"task_started is not neutral",
			rvReplays(1, rvResult, rvTaskStarted), verdictMidTurn},
		{"neutral only, ring intact: nothing in flight",
			rvReplays(1, rvProgress, rvProgress, rvUpdated), verdictIdle},
		{"neutral only, ring wrapped: unknown",
			rvReplays(5001, rvProgress, rvProgress, rvNotification), verdictUnknown},
		{"wrapped, but a result survived",
			rvReplays(5001, rvResult, rvProgress), verdictFinished},
		{"wrapped, but a tool_use survived",
			rvReplays(5001, rvAgentToolUse, rvProgress), verdictMidTurn},
		// A neutral frame in another key order misses the prefix and is
		// decoded, then skipped as an event.
		{"neutral frame in another key order",
			rvReplays(1, rvResult, `{"subtype":"task_progress","type":"system","task_id":"w1","session_id":"s1"}`), verdictFinished},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconnectVerdict(tt.replays, 0, proto).kind; got != tt.want {
				t.Errorf("verdict = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestReplayWrapped: the ring has wrapped when the replay starts past the
// first seq it was asked for, so a partial replay from lastSeq is not one.
func TestReplayWrapped(t *testing.T) {
	cases := []struct {
		name    string
		replays []shim.ServerMsg
		lastSeq int64
		want    bool
	}{
		{"full replay from seq 1", rvReplays(1, rvProgress), 0, false},
		{"full replay from seq 2", rvReplays(2, rvProgress), 0, true},
		{"partial replay resumes at lastSeq+1", rvReplays(101, rvProgress), 100, false},
		{"partial replay with a gap", rvReplays(150, rvProgress), 100, true},
		{"non-replay frames are not the first replay",
			append([]shim.ServerMsg{{Type: "cli_exited", Seq: 0}}, rvReplays(1, rvProgress)...), 0, false},
		{"empty backlog", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayWrapped(tc.replays, tc.lastSeq); got != tc.want {
				t.Errorf("replayWrapped = %v, want %v", got, tc.want)
			}
		})
	}
}

// countingProtocol counts ReadEvent calls, the decode the neutral-frame
// prefix exists to avoid.
type countingProtocol struct {
	*ClaudeProtocol
	reads *int
}

func (c countingProtocol) ReadEvent(line string) ([]clievent.Event, bool, error) {
	*c.reads++
	return c.ClaudeProtocol.ReadEvent(line)
}

func (c countingProtocol) Clone() Protocol { return c }

// TestReconnectVerdict_SkipsNeutralFramesUndecoded: a long workflow fills the
// ring with progress frames, and decoding each one on every restart is the
// cost the prefix check removes. Only the result behind them is decoded.
func TestReconnectVerdict_SkipsNeutralFramesUndecoded(t *testing.T) {
	lines := []string{rvAgentToolUse, rvResult}
	for range 2000 {
		lines = append(lines, rvProgress, rvBgChanged, rvUpdated, rvNotification)
	}
	var reads int
	v := reconnectVerdict(rvReplays(1, lines...), 0, countingProtocol{&ClaudeProtocol{}, &reads})
	if v.kind != verdictFinished {
		t.Fatalf("verdict = %d, want finished", v.kind)
	}
	if reads != 1 {
		t.Errorf("ReadEvent called %d times, want 1 (the result only)", reads)
	}

	reads = 0
	if v := reconnectVerdict(rvReplays(9001, rvProgress, rvProgress), 0, countingProtocol{&ClaudeProtocol{}, &reads}); v.kind != verdictUnknown || reads != 0 {
		t.Errorf("neutral-only wrapped backlog: verdict %d after %d decodes, want unknown after 0", v.kind, reads)
	}
}

// TestTurnNeutralLinePrefixesMatchEvents: the undecoded and decoded checks are
// one rule; a prefix whose frame decodes to a non-neutral event would skip a
// semantic frame.
func TestTurnNeutralLinePrefixesMatchEvents(t *testing.T) {
	proto := &ClaudeProtocol{}
	for _, line := range []string{rvProgress, rvBgChanged, rvUpdated, rvNotification} {
		if !isTurnNeutralLine(line) {
			t.Errorf("isTurnNeutralLine(%.60s) = false", line)
		}
		evs, _, err := proto.ReadEvent(line)
		if err != nil || len(evs) != 1 || !isTurnNeutralEvent(evs[0]) {
			t.Errorf("decoded %.60s: %v, %+v; want one neutral event", line, err, evs)
		}
	}
	for _, line := range []string{rvInit, rvTaskStarted, rvResult, rvAgentToolUse,
		`{"type":"system","subtype":"task_progress_v2","session_id":"s1"}`} {
		if isTurnNeutralLine(line) {
			t.Errorf("isTurnNeutralLine(%.60s) = true", line)
		}
	}
}

// reconnectFixture is a shim handle over net.Pipe whose far end writes the
// backlog, replay_done, then live. net.Pipe writes block until read, so
// liveRead closes only once the read loop has consumed every live frame.
type reconnectFixture struct {
	handle   *shim.ShimHandle
	srv      net.Conn
	liveRead chan struct{}
}

func newReconnectFixture(t *testing.T, helloSID string, backlog []shim.ServerMsg, live ...shim.ServerMsg) *reconnectFixture {
	t.Helper()
	client, srv := net.Pipe()
	t.Cleanup(func() { srv.Close() })
	h := &shim.ShimHandle{
		Conn: client, Reader: bufio.NewReader(client), Writer: bufio.NewWriter(client),
		Hello:      shim.ServerMsg{Type: "hello", SessionID: helloSID},
		ClientDone: make(chan struct{}),
	}
	f := &reconnectFixture{handle: h, srv: srv, liveRead: make(chan struct{})}
	frames := append(append(backlog, shim.ServerMsg{Type: "replay_done", Count: len(backlog)}), live...)
	go func() {
		for _, m := range frames {
			if f.send(m) != nil {
				return
			}
		}
		close(f.liveRead)
	}()
	return f
}

// send writes one frame; it returns once the client side has read it.
func (f *reconnectFixture) send(m shim.ServerMsg) error {
	line, _ := m.MarshalLine()
	_, err := f.srv.Write(line)
	return err
}

// drainWrites reads what the process writes (kill, detach) so it never blocks.
func (f *reconnectFixture) drainWrites() {
	go func() { _, _ = io.Copy(io.Discard, f.srv) }()
}

func waitState(t *testing.T, p *Process, want ProcessState) {
	t.Helper()
	testhelper.Eventually(t, func() bool { return p.State() == want }, 3*time.Second, "State never reached "+want.String())
}

// TestAttachReconnected_UnknownVerdictAskedBeforeReadLoop: a backlog of
// workflow frames alone after a wrapped ring is settled by the session layer's
// resolver, with the shim's session id, and an idle answer leaves the session
// Ready with nothing armed.
func TestAttachReconnected_UnknownVerdictAskedBeforeReadLoop(t *testing.T) {
	f := newReconnectFixture(t, "hello-sid", rvReplays(5001, rvProgress, rvUpdated, rvNotification))
	var asked []string
	hooks := ReconnectHooks{ResolveUnknown: func(sid string) bool { asked = append(asked, sid); return true }}
	p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, hooks)
	if err != nil {
		t.Fatalf("attachReconnected: %v", err)
	}
	f.drainWrites()
	defer p.Kill()
	if len(asked) != 1 || asked[0] != "hello-sid" {
		t.Errorf("resolver calls = %q, want one with the hello session id", asked)
	}
	if p.State() != StateReady {
		t.Errorf("State = %v, want Ready: the transcript says the turn ended", p.State())
	}
	if p.turn.reconnectedMidTurn.Load() || p.AdoptedMidTurn() {
		t.Error("mid-turn armed although the resolver said idle")
	}
}

// TestAttachReconnected_ResolverNotAskedWhenBacklogDecides: the resolver reads
// a file, so it runs only for the verdict the backlog cannot give.
func TestAttachReconnected_ResolverNotAskedWhenBacklogDecides(t *testing.T) {
	for name, backlog := range map[string][]shim.ServerMsg{
		"finished": rvReplays(5001, rvResult, rvProgress),
		"mid turn": rvReplays(5001, rvAgentToolUse, rvProgress),
		"intact":   rvReplays(1, rvProgress),
	} {
		t.Run(name, func(t *testing.T) {
			f := newReconnectFixture(t, "s1", backlog)
			hooks := ReconnectHooks{ResolveUnknown: func(string) bool { t.Error("resolver asked"); return true }}
			p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, hooks)
			if err != nil {
				t.Fatalf("attachReconnected: %v", err)
			}
			f.drainWrites()
			p.Kill()
		})
	}
}

// TestAttachReconnected_UnknownMidTurnThenLiveResult: a resolver that cannot
// see an ended turn leaves the session Running and armed, and the late result
// still settles it.
func TestAttachReconnected_UnknownMidTurnThenLiveResult(t *testing.T) {
	f := newReconnectFixture(t, "s1", rvReplays(5001, rvProgress, rvProgress))
	hooks := ReconnectHooks{ResolveUnknown: func(string) bool { return false }}
	p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, hooks)
	if err != nil {
		t.Fatalf("attachReconnected: %v", err)
	}
	defer p.Kill()
	if p.State() != StateRunning || !p.AdoptedMidTurn() {
		t.Fatalf("State = %v, AdoptedMidTurn = %v; want Running and armed", p.State(), p.AdoptedMidTurn())
	}
	if err := f.send(shim.ServerMsg{Type: "stdout", Seq: 5003, Line: rvResult}); err != nil {
		t.Fatal(err)
	}
	f.drainWrites()
	waitState(t, p, StateReady)
}

// TestAttachReconnected_LiveResultRightAfterReadLoopStarts is #1778 for the
// unknown verdict: the result is the read loop's very first frame, so the
// resolver's answer must already be armed when the loop starts. The resolver
// waits a moment for that frame to be consumed; it cannot be, unless the loop
// is already running.
func TestAttachReconnected_LiveResultRightAfterReadLoopStarts(t *testing.T) {
	f := newReconnectFixture(t, "s1", rvReplays(5001, rvProgress, rvProgress),
		shim.ServerMsg{Type: "stdout", Seq: 5003, Line: rvResult})
	hooks := ReconnectHooks{ResolveUnknown: func(string) bool {
		select {
		case <-f.liveRead:
			t.Error("the live result was read before the verdict was armed")
		case <-time.After(150 * time.Millisecond):
		}
		return false
	}}
	p, _, err := (&Wrapper{}).attachReconnected(context.Background(), f.handle, "k", 0, &ClaudeProtocol{}, 0, 0, hooks)
	if err != nil {
		t.Fatalf("attachReconnected: %v", err)
	}
	defer p.Kill()
	<-f.liveRead
	f.drainWrites()
	waitState(t, p, StateReady)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := p.AdoptedOutcome(ctx); err != nil || out.Result.Text != "ok" {
		t.Errorf("adopted outcome = %+v, %v; want the live result", out, err)
	}
}
