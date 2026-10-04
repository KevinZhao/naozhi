package session

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const adoptAssistantLine = `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"s1"}`

// fakeShimPID is what the fake shim's hello reports. No process can have it,
// so Kill's SIGUSR2 to the shim reaches nothing instead of the test binary.
const fakeShimPID = 1 << 30

// reconnectToFakeShim runs ReconnectShimsCtx against a fake shim for key that
// replays backlog, numbered from firstSeq, and returns the reattached process
// plus a func that sends a live stdout line on the same connection — the late
// result of the turn.
func reconnectToFakeShim(t *testing.T, key string, firstSeq int64, backlog []string) (*Router, *cli.Process, func(line string)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	// t.TempDir() on darwin overflows the 104-byte sun_path limit.
	dir, err := os.MkdirTemp("/tmp", "nz-adopt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	w.ShimManager = mgr
	r := NewRouter(RouterConfig{Wrapper: w})
	t.Cleanup(r.Shutdown)

	sess := injectSession(r, key, nil)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	attached := make(chan net.Conn, 1)
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadBytes('\n'); err != nil { // attach
			return
		}
		frames := []shim.ServerMsg{{Type: "hello", ProtocolVersion: shim.ProtocolVersion, ShimPID: fakeShimPID}}
		for i, line := range backlog {
			frames = append(frames, shim.ServerMsg{Type: "replay", Seq: firstSeq + int64(i), Line: line})
		}
		frames = append(frames, shim.ServerMsg{Type: "replay_done", Count: len(backlog)})
		for i := range frames {
			data, err := frames[i].MarshalLine()
			if err != nil {
				return
			}
			if _, err := conn.Write(data); err != nil {
				conn.Close()
				return
			}
		}
		attached <- conn
		io.Copy(io.Discard, rd) //nolint:errcheck // nothing the client writes needs an answer
	}()
	// ShimPID is this test process: it passes the liveness and binary-identity
	// gates. The socket must exist before ReconnectShimsCtx runs, or Discover
	// treats the shim as a zombie and SIGTERMs that PID, i.e. the test binary.
	state := shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: key, Backend: "claude",
		CLIArgs:      driftArgsFor(r).driftCompareArgs(w, "claude", key, sess, &shim.SpawnOverlay{}),
		SpawnOverlay: &shim.SpawnOverlay{},
	}
	if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(key)), state); err != nil {
		t.Fatal(err)
	}

	r.ReconnectShimsCtx(context.Background())

	proc, ok := sess.loadProcess().(*cli.Process)
	if !ok || proc == nil {
		t.Fatalf("session process after reconnect = %T, want the reattached *cli.Process", sess.loadProcess())
	}
	var conn net.Conn
	select {
	case conn = <-attached:
		t.Cleanup(func() { conn.Close() })
	case <-time.After(5 * time.Second):
		t.Fatal("fake shim never finished the attach handshake")
	}
	emit := func(line string) {
		t.Helper()
		data, err := (&shim.ServerMsg{Type: "stdout", Seq: firstSeq + int64(len(backlog)), Line: line}).MarshalLine()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	return r, proc, emit
}

// TestAdoptInFlight_MidTurnLatchStaysAdoptableOnceResolved: the late result of
// a mid-turn reconnect can land in the gap between ReconnectShimsCtx and cron's
// reconcile. That turn is still the run cron is asking about, and its answer
// sits in the latch, so the verdict must stay AdoptLive — otherwise a run that
// finished is recorded as interrupted and its answer is dropped.
func TestAdoptInFlight_MidTurnLatchStaysAdoptableOnceResolved(t *testing.T) {
	const key = "cron:09a61c45ad4c76ba"
	r, proc, emit := reconnectToFakeShim(t, key, 1, []string{adoptAssistantLine})

	if !proc.AdoptedTurnPending() {
		t.Fatal("a backlog ending mid-turn did not arm a pending latch")
	}
	// No watermark: a run the old binary started, or one that never took one.
	// A mid-turn latch is adopted without it.
	if p, state := r.AdoptInFlight(key, cli.TurnWatermark{}, false); state != AdoptLive || p != proc {
		t.Fatalf("before the result: AdoptInFlight = (%p, %v), want (%p, AdoptLive)", p, state, proc)
	}

	emit(`{"type":"result","subtype":"success","result":"late answer","session_id":"s1"}`)
	testhelper.Eventually(t, func() bool { return !proc.AdoptedTurnPending() },
		5*time.Second, "the live result never reached the latch")

	p, state := r.AdoptInFlight(key, cli.TurnWatermark{}, false)
	if state != AdoptLive || p != proc {
		t.Fatalf("after the result: AdoptInFlight = (%p, %v), want (%p, AdoptLive); "+
			"a turn that finished before cron asked must still be adopted", p, state, proc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("AdoptedOutcome: %v", err)
	}
	if out.End != cli.AdoptedEndResult || out.Result.Text != "late answer" {
		t.Errorf("outcome = %+v, want the late result", out)
	}
}

// TestAdoptInFlight_ReplayedResultIsNotAdopted is the boundary of the gate. A
// reconnect replays the shim's whole backlog, so one that ends in a result arms
// a latch on every idle reconnect — with the previous turn's answer, which the
// old process already delivered. With no watermark nothing shows it came after
// this run's Send, so it must stay AdoptNone rather than be recorded as this
// run's success.
func TestAdoptInFlight_ReplayedResultIsNotAdopted(t *testing.T) {
	const key = "cron:5d1e0c2b7a9f4e83"
	r, proc, _ := reconnectToFakeShim(t, key, 1, []string{
		adoptAssistantLine,
		`{"type":"result","subtype":"success","result":"previous run","session_id":"s1"}`,
	})

	// The latch is armed and holds the replayed result: the verdict below is
	// the gate's decision, not a reconnect that latched nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := proc.AdoptedOutcome(ctx); err != nil || out.Result.Text != "previous run" {
		t.Fatalf("AdoptedOutcome = (%+v, %v), want the replayed result latched", out, err)
	}
	if p, state := r.AdoptInFlight(key, cli.TurnWatermark{}, false); state != AdoptNone || p != nil {
		t.Errorf("AdoptInFlight = (%p, %v), want (nil, AdoptNone) for a latch armed from the replay", p, state)
	}
}

// TestAdoptInFlight_ResultAfterTheSendWatermarkIsAdopted is the turn that ended
// while naozhi was down: the run took its watermark at seq 5, right after the
// previous turn's result, and its own result is the backlog's last frame. That
// one is adopted, and the latch holds it rather than the earlier result.
func TestAdoptInFlight_ResultAfterTheSendWatermarkIsAdopted(t *testing.T) {
	const key = "cron:7c2e94d01b5a6f38"
	r, proc, _ := reconnectToFakeShim(t, key, 5, []string{
		`{"type":"result","subtype":"success","result":"previous run","session_id":"s1"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do thing"}]},"session_id":"s1"}`,
		adoptAssistantLine,
		`{"type":"result","subtype":"success","result":"this run","session_id":"s1"}`,
	})

	p, state := r.AdoptInFlight(key, cli.TurnWatermark{ShimPID: fakeShimPID, Seq: 5}, true)
	if state != AdoptLive || p != proc {
		t.Fatalf("AdoptInFlight = (%p, %v), want (%p, AdoptLive) for a result past the watermark", p, state, proc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := p.AdoptedOutcome(ctx)
	if err != nil {
		t.Fatalf("AdoptedOutcome: %v", err)
	}
	if out.End != cli.AdoptedEndResult || out.Result.Text != "this run" {
		t.Errorf("outcome = %+v, want the result at seq 8", out)
	}
}

// TestAdoptInFlight_WatermarkAfterReconnectRejectsTheReplayedResult: a run
// started on a process that reattached to an idle shim takes its watermark
// before any live frame arrives. It must already sit past the replayed result,
// or a restart before this run's own result would adopt the previous turn's.
func TestAdoptInFlight_WatermarkAfterReconnectRejectsTheReplayedResult(t *testing.T) {
	const key = "cron:e41b07c9d26a5f83"
	r, _, _ := reconnectToFakeShim(t, key, 1, []string{
		adoptAssistantLine,
		`{"type":"result","subtype":"success","result":"previous run","session_id":"s1"}`,
	})

	w, ok := r.ss.Load(key).TurnWatermark()
	if !ok || w != (cli.TurnWatermark{ShimPID: fakeShimPID, Seq: 2}) {
		t.Fatalf("TurnWatermark = (%+v, %v), want ({%d 2}, true): the replayed frames were received",
			w, ok, fakeShimPID)
	}
	if p, state := r.AdoptInFlight(key, w, true); state != AdoptNone || p != nil {
		t.Errorf("AdoptInFlight = (%p, %v), want (nil, AdoptNone) for the result the watermark already covers", p, state)
	}
}

// TestTurnWatermark_NoneWhileATurnIsRunning: a Send issued now would queue
// behind the running turn, and that turn's result lands past any watermark
// taken now. A restart before it arrives would then adopt that result as the
// caller's run, so there is no watermark until the session is idle again.
func TestTurnWatermark_NoneWhileATurnIsRunning(t *testing.T) {
	const key = "cron:3f8a1d6c07b2e954"
	r, proc, emit := reconnectToFakeShim(t, key, 1, []string{adoptAssistantLine})
	sess := r.ss.Load(key)

	if !proc.IsRunning() {
		t.Fatal("a backlog ending mid-turn did not leave the process running")
	}
	if w, ok := sess.TurnWatermark(); ok {
		t.Errorf("TurnWatermark mid-turn = (%+v, true), want none", w)
	}

	emit(`{"type":"result","subtype":"success","result":"done","session_id":"s1"}`)
	testhelper.Eventually(t, func() bool { return !proc.IsRunning() },
		5*time.Second, "the live result never ended the turn")
	if w, ok := sess.TurnWatermark(); !ok || w != (cli.TurnWatermark{ShimPID: fakeShimPID, Seq: 2}) {
		t.Errorf("TurnWatermark once idle = (%+v, %v), want ({%d 2}, true)", w, ok, fakeShimPID)
	}
}

// TestTurnWatermark_NoneWhileASendIsQueued: a Send waiting on (or holding)
// sendMu runs before the caller's, with the same effect as a running turn.
func TestTurnWatermark_NoneWhileASendIsQueued(t *testing.T) {
	const key = "cron:a90c5e27d14b3f68"
	r, _, _ := reconnectToFakeShim(t, key, 1, []string{
		adoptAssistantLine,
		`{"type":"result","subtype":"success","result":"previous run","session_id":"s1"}`,
	})
	sess := r.ss.Load(key)

	sess.turnWaiters.Add(1)
	if w, ok := sess.TurnWatermark(); ok {
		t.Errorf("TurnWatermark with a Send queued = (%+v, true), want none", w)
	}
	sess.turnWaiters.Add(-1)
	if _, ok := sess.TurnWatermark(); !ok {
		t.Error("TurnWatermark on an idle session = none, want one")
	}
}

// TestTurnWatermark_NoneWhilePassthroughOwesAResult: a passthrough message
// already written to the CLI is a turn it will run before the caller's, though
// the process is not marked running until the CLI picks it up.
func TestTurnWatermark_NoneWhilePassthroughOwesAResult(t *testing.T) {
	const key = "cron:6be27d903a1f5c48"
	r, proc, _ := reconnectToFakeShim(t, key, 1, []string{
		adoptAssistantLine,
		`{"type":"result","subtype":"success","result":"previous run","session_id":"s1"}`,
	})
	sess := r.ss.Load(key)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		proc.SendPassthrough(ctx, "queued", nil, nil, "") //nolint:errcheck // canceled below
	}()
	t.Cleanup(func() { cancel(); <-done })
	testhelper.Eventually(t, func() bool { return proc.PassthroughDepth() == 1 },
		5*time.Second, "the passthrough message was never queued")

	if proc.IsRunning() {
		t.Fatal("the process is running; this test needs a queued, not-yet-started turn")
	}
	if w, ok := sess.TurnWatermark(); ok {
		t.Errorf("TurnWatermark with a passthrough result owed = (%+v, true), want none", w)
	}
}
