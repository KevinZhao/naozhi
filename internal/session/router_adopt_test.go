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

// reconnectToFakeShim runs ReconnectShimsCtx against a fake shim for key that
// replays backlog, and returns the reattached process plus a func that sends a
// live stdout line on the same connection — the late result of the turn.
func reconnectToFakeShim(t *testing.T, key string, backlog []string) (*Router, *cli.Process, func(line string)) {
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
		frames := []shim.ServerMsg{{Type: "hello", ProtocolVersion: shim.ProtocolVersion}}
		for i, line := range backlog {
			frames = append(frames, shim.ServerMsg{Type: "replay", Seq: int64(i + 1), Line: line})
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
		data, err := (&shim.ServerMsg{Type: "stdout", Seq: int64(len(backlog) + 1), Line: line}).MarshalLine()
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
	r, proc, emit := reconnectToFakeShim(t, key, []string{adoptAssistantLine})

	if !proc.AdoptedTurnPending() {
		t.Fatal("a backlog ending mid-turn did not arm a pending latch")
	}
	if p, state := r.AdoptInFlight(key); state != AdoptLive || p != proc {
		t.Fatalf("before the result: AdoptInFlight = (%p, %v), want (%p, AdoptLive)", p, state, proc)
	}

	emit(`{"type":"result","subtype":"success","result":"late answer","session_id":"s1"}`)
	testhelper.Eventually(t, func() bool { return !proc.AdoptedTurnPending() },
		5*time.Second, "the live result never reached the latch")

	p, state := r.AdoptInFlight(key)
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
// old process already delivered. Nothing shows it came after this run's Send,
// so it must stay AdoptNone rather than be recorded as this run's success.
func TestAdoptInFlight_ReplayedResultIsNotAdopted(t *testing.T) {
	const key = "cron:5d1e0c2b7a9f4e83"
	r, proc, _ := reconnectToFakeShim(t, key, []string{
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
	if p, state := r.AdoptInFlight(key); state != AdoptNone || p != nil {
		t.Errorf("AdoptInFlight = (%p, %v), want (nil, AdoptNone) for a latch armed from the replay", p, state)
	}
}
