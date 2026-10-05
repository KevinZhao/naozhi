package session

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// resetShimFake is a shim bound on key's socket whose hello reports cliAlive.
// It records the client messages and, on shutdown, closes its listener, which
// unlinks the socket the way a real shim's exit does. With lateFrames it
// writes the frames after the hello only once the client has hung up.
type resetShimFake struct {
	ln         net.Listener
	socket     string
	cliAlive   bool
	lateFrames bool

	mu       sync.Mutex
	accepted int
	received []string
	conns    sync.WaitGroup
}

// newResetShimRouter returns a Router whose wrapper has a real shim manager,
// with a fake shim for key listening on the key's socket and a state file
// naming it. Not parallel-safe: it sets XDG_RUNTIME_DIR.
func newResetShimRouter(t *testing.T, key string, cliAlive, lateFrames bool) (*Router, *resetShimFake) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shim retire needs unix sockets and unix PID liveness")
	}
	// t.TempDir() on darwin overflows the 104-byte sun_path limit.
	dir, err := os.MkdirTemp("/tmp", "nz-rst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
	w.ShimManager = mgr
	r := NewRouter(RouterConfig{Wrapper: w})
	t.Cleanup(r.Shutdown)

	socket := shim.SocketPath(shim.KeyHash(key))
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &resetShimFake{ln: ln, socket: socket, cliAlive: cliAlive, lateFrames: lateFrames}
	t.Cleanup(func() {
		_ = ln.Close()
		f.conns.Wait()
	})
	f.conns.Add(1)
	go f.serve()

	// ShimPID is this test process: it passes the liveness and binary-identity
	// checks. The hello reports FakeShimPID, which no process has.
	token := []byte("reset-retire-token")
	state := shim.State{
		ShimPID: os.Getpid(), Socket: socket, Key: key, Backend: "claude",
		AuthToken: base64.StdEncoding.EncodeToString(token),
	}
	if err := shim.WriteStateFile(shim.StateFilePath(dir, shim.KeyHash(key)), state); err != nil {
		t.Fatal(err)
	}
	return r, f
}

func (f *resetShimFake) serve() {
	defer f.conns.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.accepted++
		f.mu.Unlock()
		f.conns.Add(1)
		go func() {
			defer f.conns.Done()
			defer conn.Close()
			f.handle(conn)
		}()
	}
}

func (f *resetShimFake) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReader(conn)
	if _, err := rd.ReadBytes('\n'); err != nil { // attach
		return
	}
	alive := f.cliAlive
	frames := []shim.ServerMsg{
		{Type: "hello", ProtocolVersion: shim.ProtocolVersion, ShimPID: FakeShimPID, CLIAlive: &alive},
		{Type: "replay_done"},
	}
	if !alive {
		code := 1
		frames = append(frames, shim.ServerMsg{Type: "cli_exited", Code: &code})
	}
	// Write errors are ignored, as the real shim's writeRaw does: the retire
	// client hangs up right after the hello, so a later frame can hit EPIPE
	// while its shutdown line is still queued for reading.
	for i := range frames {
		if i == 1 && f.lateFrames {
			queued, _ := io.ReadAll(rd)
			rd = bufio.NewReader(bytes.NewReader(queued))
		}
		data, err := frames[i].MarshalLine()
		if err != nil {
			return
		}
		_, _ = conn.Write(data)
	}
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg shim.ClientMsg
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, msg.Type)
		f.mu.Unlock()
		if msg.Type == "shutdown" {
			_ = f.ln.Close()
			return
		}
	}
}

// stop closes the listener, waits for every connection to end and returns the
// connection count and the message types the clients sent.
func (f *resetShimFake) stop() (int, []string) {
	_ = f.ln.Close()
	f.conns.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted, append([]string(nil), f.received...)
}

func shimStuckFlag(r *Router, key string) (stuck bool) {
	r.ss.Update(func(tx sessTx) { stuck = tx.Ext().spawns.ShimStuck(key) })
	return stuck
}

// TestReset_RetiresDeadCLIShim: /new after a CLI crash finds the crashed
// session's shim still bound for its post-exit reattach window. Reset asks that
// shim to shut down instead of waiting the window out, so the key is not
// flagged shim-stuck and the socket is free for the next spawn.
func TestReset_RetiresDeadCLIShim(t *testing.T) {
	for _, tc := range []struct {
		name       string
		proc       processIface
		lateFrames bool
	}{
		{"dead process", newDeadProc(), false},
		{"no process", nil, false},
		// The client reads only the hello, then sends shutdown and closes.
		{"client hangs up after hello", newDeadProc(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "feishu:direct:alice:general"
			r, f := newResetShimRouter(t, key, false, tc.lateFrames)
			injectSession(r, key, tc.proc)

			r.Reset(key)

			if shimStuckFlag(r, key) {
				t.Error("key flagged shim-stuck after Reset of a crashed session; the dead-CLI shim should have been retired")
			}
			if _, err := os.Lstat(f.socket); !os.IsNotExist(err) {
				t.Errorf("socket after Reset: Lstat err = %v, want not-exist", err)
			}
			if _, got := f.stop(); len(got) != 1 || got[0] != "shutdown" {
				t.Errorf("messages the shim received = %v, want [shutdown]", got)
			}
		})
	}
}

// TestReset_LeavesLiveCLIShimRunning: a dead process whose shim still reports a
// live CLI (naozhi lost the link, not the CLI) is only probed; nothing is sent,
// and the socket wait and shim-stuck flag behave as before.
func TestReset_LeavesLiveCLIShimRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits out the 2s socket wait")
	}
	const key = "feishu:direct:bob:general"
	r, f := newResetShimRouter(t, key, true, false)
	injectSession(r, key, newDeadProc())

	r.Reset(key)

	if !shimStuckFlag(r, key) {
		t.Error("key not flagged shim-stuck although its live-CLI shim kept the socket bound")
	}
	if _, got := f.stop(); len(got) != 0 {
		t.Errorf("messages the live-CLI shim received = %v, want none", got)
	}
}

// TestReset_LiveProcessIsClosedNotProbed: a live process is released through
// Close as before; the retire probe never dials its shim.
func TestReset_LiveProcessIsClosedNotProbed(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits out the 2s socket wait")
	}
	const key = "feishu:direct:carol:general"
	r, f := newResetShimRouter(t, key, false, false)
	proc := newIdleProc()
	injectSession(r, key, proc)

	r.Reset(key)

	if proc.Alive() {
		t.Error("live process not closed by Reset")
	}
	if accepted, got := f.stop(); accepted != 0 {
		t.Errorf("shim accepted %d connections (messages %v) during the Reset of a live process, want 0", accepted, got)
	}
}
