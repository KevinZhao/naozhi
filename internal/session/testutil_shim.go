//go:build !release

package session

import (
	"bufio"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// FakeShimPID is what the fake shim's hello reports. No process can have it,
// so Kill's SIGUSR2 to the shim reaches nothing instead of the test binary.
const FakeShimPID = 1 << 30

// FakeShim serves one key's shim socket: it replays a backlog, numbered from
// a first seq, to the client that attaches and then holds the connection open.
// A router built from Config finds it on ReconnectShimsCtx and reattaches the
// key to a real *cli.Process, which tests outside this package need to reach
// the adoption gate.
type FakeShim struct {
	Config   RouterConfig
	next     int64
	attached chan net.Conn
	conn     net.Conn
}

// StartFakeShimForTest starts the shim for key and records it in a state file
// the way a surviving shim would. It calls t.Setenv, so tests using it cannot
// call t.Parallel.
func StartFakeShimForTest(t testing.TB, key string, firstSeq int64, backlog []string) *FakeShim {
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
	f := &FakeShim{
		Config:   RouterConfig{Wrapper: w},
		next:     firstSeq + int64(len(backlog)),
		attached: make(chan net.Conn, 1),
	}

	t.Setenv("XDG_RUNTIME_DIR", dir)
	socket := shim.SocketPath(shim.KeyHash(key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
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
		frames := []shim.ServerMsg{{Type: "hello", ProtocolVersion: shim.ProtocolVersion, ShimPID: FakeShimPID}}
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
		f.attached <- conn
		io.Copy(io.Discard, rd) //nolint:errcheck // nothing the client writes needs an answer
	}()
	// ShimPID is this test process: it passes the liveness and binary-identity
	// gates. The socket must exist before ReconnectShimsCtx runs, or Discover
	// treats the shim as a zombie and SIGTERMs that PID, i.e. the test binary.
	// An empty CLIArgs never reads as argv drift.
	state := shim.State{ShimPID: os.Getpid(), Socket: socket, Key: key, Backend: "claude", SpawnOverlay: &shim.SpawnOverlay{}}
	if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(key)), state); err != nil {
		t.Fatal(err)
	}
	return f
}

// Attached waits for the replay to reach the client ReconnectShimsCtx attached.
func (f *FakeShim) Attached(t testing.TB) {
	t.Helper()
	select {
	case f.conn = <-f.attached:
		t.Cleanup(func() { f.conn.Close() })
	case <-time.After(5 * time.Second):
		t.Fatal("fake shim never finished the attach handshake")
	}
}

// Emit sends a live stdout line, numbered after the backlog and any earlier
// Emit, on the connection Attached waited for.
func (f *FakeShim) Emit(t testing.TB, line string) {
	t.Helper()
	if f.conn == nil {
		t.Fatal("FakeShim.Emit called before Attached")
		return
	}
	data, err := (&shim.ServerMsg{Type: "stdout", Seq: f.next, Line: line}).MarshalLine()
	if err != nil {
		t.Fatal(err)
	}
	f.next++
	if _, err := f.conn.Write(data); err != nil {
		t.Fatal(err)
	}
}
