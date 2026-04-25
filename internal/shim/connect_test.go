package shim

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeShimServer listens on a Unix socket and handles exactly one connection
// according to the specified behaviour.
type fakeShimServer struct {
	ln      net.Listener
	path    string
	cleanup func()
}

func newFakeShimServer(t *testing.T) *fakeShimServer {
	t.Helper()
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "fake.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return &fakeShimServer{
		ln:   ln,
		path: path,
		cleanup: func() {
			ln.Close()
			os.Remove(path)
		},
	}
}

// handleOnce accepts one connection and executes fn(conn), then returns.
func (f *fakeShimServer) handleOnce(fn func(conn net.Conn)) {
	go func() {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fn(conn)
	}()
}

// serveHello accepts one connection, reads an attach message, and responds with hello.
func (f *fakeShimServer) serveHello(t *testing.T, token []byte) {
	f.handleOnce(func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

		// Read attach
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return
		}
		var attach ClientMsg
		if err := json.Unmarshal(line, &attach); err != nil {
			return
		}

		// Verify token
		clientToken, err := base64.StdEncoding.DecodeString(attach.Token)
		if err != nil || string(clientToken) != string(token) {
			hello := ServerMsg{Type: "auth_failed", Msg: "invalid token"}
			data, _ := hello.MarshalLine()
			conn.Write(append(data, '\n')) //nolint:errcheck
			return
		}

		// Send hello
		hello := ServerMsg{
			Type:            "hello",
			ShimPID:         os.Getpid(),
			CLIPID:          os.Getpid() + 1,
			CLIAlive:        boolPtr(true),
			ProtocolVersion: ProtocolVersion,
		}
		data, _ := hello.MarshalLine()
		conn.Write(append(data, '\n')) //nolint:errcheck

		// Keep alive until closed by client
		time.Sleep(100 * time.Millisecond)
	})
}

// --- connect ---

func TestConnect_Success(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()

	token := []byte("test-token-32-bytes-padded!!!!")
	srv.serveHello(t, token)

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	handle, err := m.connect(srv.path, token, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer handle.Close()

	if handle.Hello.Type != "hello" {
		t.Errorf("Hello.Type = %q, want hello", handle.Hello.Type)
	}
	if handle.Hello.ProtocolVersion != ProtocolVersion {
		t.Errorf("Hello.ProtocolVersion = %d, want %d", handle.Hello.ProtocolVersion, ProtocolVersion)
	}
	if handle.Conn == nil {
		t.Error("Conn should not be nil")
	}
	if handle.ClientDone == nil {
		t.Error("ClientDone should not be nil")
	}
}

func TestConnect_WrongToken_AuthFailed(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()

	realToken := []byte("real-token-32bytes-padded!!!!!!")
	wrongToken := []byte("wrong-token-32bytes-padded!!!!!")

	srv.serveHello(t, realToken)

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	_, err := m.connect(srv.path, wrongToken, 0)
	if err == nil {
		t.Fatal("expected error for wrong token, got nil")
	}
}

func TestConnect_AuthFailed_Response(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()

	// Server immediately sends auth_failed
	srv.handleOnce(func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		// Consume the attach message
		bufio.NewReader(conn).ReadBytes('\n') //nolint:errcheck
		msg := ServerMsg{Type: "auth_failed", Msg: "bad credentials"}
		data, _ := msg.MarshalLine()
		conn.Write(append(data, '\n')) //nolint:errcheck
	})

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	token := []byte("some-32-byte-token-padded!!!!!!!")
	_, err := m.connect(srv.path, token, 0)
	if err == nil {
		t.Fatal("expected error for auth_failed, got nil")
	}
}

func TestConnect_UnexpectedMessageType(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()

	// Server sends an unexpected message type
	srv.handleOnce(func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		bufio.NewReader(conn).ReadBytes('\n') //nolint:errcheck
		msg := ServerMsg{Type: "stdout", Line: "oops"}
		data, _ := msg.MarshalLine()
		conn.Write(append(data, '\n')) //nolint:errcheck
	})

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	token := []byte("some-32-byte-token-padded!!!!!!!")
	_, err := m.connect(srv.path, token, 0)
	if err == nil {
		t.Fatal("expected error for unexpected message type, got nil")
	}
}

func TestConnect_BadJSON_HelloLine(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()

	// Server sends bad JSON after attach
	srv.handleOnce(func(conn net.Conn) {
		conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		bufio.NewReader(conn).ReadBytes('\n') //nolint:errcheck
		conn.Write([]byte("not json at all\n")) //nolint:errcheck
	})

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	token := []byte("some-32-byte-token-padded!!!!!!!")
	_, err := m.connect(srv.path, token, 0)
	if err == nil {
		t.Fatal("expected error for bad JSON hello, got nil")
	}
}

func TestConnect_DialFailure(t *testing.T) {
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	_, err := m.connect("/nonexistent/path/shim.sock", []byte("token"), 0)
	if err == nil {
		t.Fatal("expected error for non-existent socket, got nil")
	}
}

func TestConnect_ServerClosesBeforeHello(t *testing.T) {
	srv := newFakeShimServer(t)
	defer srv.cleanup()

	// Server closes connection immediately after accepting
	srv.handleOnce(func(conn net.Conn) {
		bufio.NewReader(conn).ReadBytes('\n') //nolint:errcheck
		conn.Close()
	})

	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	token := []byte("some-32-byte-token-padded!!!!!!!")
	_, err := m.connect(srv.path, token, 0)
	if err == nil {
		t.Fatal("expected error when server closes before hello, got nil")
	}
}

// --- Discover ---

func TestDiscover_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	states, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states, got %d", len(states))
	}
}

func TestDiscover_NonExistentDir(t *testing.T) {
	dir := t.TempDir()
	nonExistent := filepath.Join(dir, "does_not_exist")
	m := mustNewManager(t, ManagerConfig{StateDir: nonExistent})
	states, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states for non-existent dir, got %d", len(states))
	}
}

func TestDiscover_SkipsNonJSONFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a non-.json file
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not a state"), 0600) //nolint:errcheck

	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	states, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states (skipping non-json), got %d", len(states))
	}
}

func TestDiscover_RemovesCorruptStateFile(t *testing.T) {
	dir := t.TempDir()

	// Write a corrupt state file
	corruptPath := filepath.Join(dir, "corrupt.json")
	os.WriteFile(corruptPath, []byte("bad json {{{"), 0600) //nolint:errcheck

	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	states, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states for corrupt file, got %d", len(states))
	}
	// Corrupt file should have been removed
	if _, statErr := os.Stat(corruptPath); statErr == nil {
		t.Error("corrupt state file should have been removed")
	}
}

func TestDiscover_RemovesStateWithDeadPID(t *testing.T) {
	dir := t.TempDir()

	// Write a state file with a PID that definitely doesn't exist
	// PID 999999999 is virtually guaranteed not to exist
	state := State{
		ShimPID:   999999999,
		Socket:    "/tmp/nonexistent.sock",
		AuthToken: "dA==",
		Key:       "test:dead",
	}
	path := filepath.Join(dir, KeyHash("test:dead")+".json")
	if err := WriteStateFile(path, state); err != nil {
		t.Fatal(err)
	}

	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	states, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states (dead PID), got %d", len(states))
	}
	// State file should have been removed
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("stale state file should have been removed")
	}
}

// --- StopAll ---

func TestManager_StopAll_SendsShutdown(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})

	const n = 2
	type pair struct {
		client net.Conn
		server net.Conn
	}
	pairs := make([]pair, n)
	for i := range pairs {
		c, s := net.Pipe()
		pairs[i] = pair{c, s}
		handle := &ShimHandle{
			Conn:       c,
			Reader:     bufio.NewReader(c),
			Writer:     bufio.NewWriter(c),
			ClientDone: make(chan struct{}),
		}
		m.mu.Lock()
		m.shims[string(rune('A'+i))] = handle
		m.mu.Unlock()
	}

	msgCh := make(chan ClientMsg, n)
	for i := range pairs {
		go func(srv net.Conn) {
			srv.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
			line, err := bufio.NewReader(srv).ReadBytes('\n')
			srv.Close()
			if err != nil {
				return
			}
			var msg ClientMsg
			json.Unmarshal(line, &msg) //nolint:errcheck
			msgCh <- msg
		}(pairs[i].server)
	}

	done := make(chan struct{})
	go func() {
		m.StopAll(t.Context())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAll did not complete in time")
	}

	received := 0
	timeout := time.After(2 * time.Second)
	for received < n {
		select {
		case msg := <-msgCh:
			if msg.Type != "shutdown" {
				t.Errorf("got %q, want shutdown", msg.Type)
			}
			received++
		case <-timeout:
			t.Fatalf("timeout: only received %d/%d shutdown messages", received, n)
		}
	}
}

// --- cliProc ---

func TestCLIProc_AliveAndPID(t *testing.T) {
	cli, err := startCLI("sh", []string{"-c", "sleep 10"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	defer cli.kill()

	if !cli.alive() {
		t.Error("cli.alive() = false, want true for running process")
	}
	if cli.pid() <= 0 {
		t.Errorf("cli.pid() = %d, want >0", cli.pid())
	}
}

func TestCLIProc_Kill_TransitionsToNotAlive(t *testing.T) {
	cli, err := startCLI("sh", []string{"-c", "sleep 60"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}

	cli.kill()

	select {
	case <-cli.exited:
		// expected
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit after kill")
	}

	if cli.alive() {
		t.Error("cli.alive() = true after kill, want false")
	}
}

func TestCLIProc_Wait_Idempotent(t *testing.T) {
	cli, err := startCLI("sh", []string{"-c", "exit 0"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}

	// Call wait twice; must not panic
	cli.wait()
	cli.wait()

	select {
	case <-cli.exited:
		// good
	default:
		t.Error("exited channel should be closed after wait")
	}
}

func TestCLIProc_CloseStdin(t *testing.T) {
	// "sh -c exit" exits immediately once started; just verify closeStdin doesn't panic.
	cli, err := startCLI("sh", []string{"-c", "sleep 60"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	defer cli.kill()

	// closeStdin must not panic on a live or dead process
	cli.closeStdin()
	cli.closeStdin() // idempotent
}

func TestCLIProc_WaitOrKill_ExitsCleanly(t *testing.T) {
	// Use "exit 0" which exits immediately.
	// Start wait() in a goroutine to close exited so waitOrKill
	// returns via the clean path rather than timing out.
	cli, err := startCLI("sh", []string{"-c", "exit 0"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}

	go cli.wait() // reap the process so exited channel closes
	cli.waitOrKill(3 * time.Second)

	if cli.alive() {
		t.Error("cli.alive() = true after waitOrKill, want false")
	}
}

func TestCLIProc_WaitOrKill_KillsIfTimeout(t *testing.T) {
	// Use "sleep 60" which will never exit on its own
	cli, err := startCLI("sh", []string{"-c", "sleep 60"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}

	// Very short timeout forces kill
	start := time.Now()
	cli.waitOrKill(100 * time.Millisecond)
	elapsed := time.Since(start)

	if cli.alive() {
		t.Error("cli.alive() = true after waitOrKill with short timeout")
	}
	if elapsed > 3*time.Second {
		t.Errorf("waitOrKill took %v, expected ~100ms", elapsed)
	}
}

func TestCLIProc_Interrupt(t *testing.T) {
	cli, err := startCLI("sh", []string{"-c", "sleep 60"}, "")
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	defer cli.kill()

	// interrupt on a live process must not panic
	cli.interrupt()

	// interrupt on a dead process must not panic
	cli.kill()
	cli.interrupt()
}

func TestStartCLI_BadPath(t *testing.T) {
	_, err := startCLI("/nonexistent/binary", nil, "")
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
}

func TestStartCLI_BadCWD(t *testing.T) {
	// CWD that doesn't exist — some OS accept this at exec time
	cli, err := startCLI("sh", []string{"-c", "exit 0"}, "/nonexistent/cwd")
	if err != nil {
		// Some systems reject bad CWD at start time
		return
	}
	cli.wait()
	cli.kill()
}
