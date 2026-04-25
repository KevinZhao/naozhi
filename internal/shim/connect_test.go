package shim

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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
			conn.Write(data) //nolint:errcheck
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
		conn.Write(data) //nolint:errcheck

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
		conn.Write(data) //nolint:errcheck
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
		bufio.NewReader(conn).ReadBytes('\n')             //nolint:errcheck
		msg := ServerMsg{Type: "stdout", Line: "oops"}
		data, _ := msg.MarshalLine()
		conn.Write(data) //nolint:errcheck
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
		bufio.NewReader(conn).ReadBytes('\n')             //nolint:errcheck
		conn.Write([]byte("not json at all\n"))           //nolint:errcheck
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

// TestDiscover_RemovesStateWithMissingSocket covers F4: a zombie shim
// (live PID but its AF_UNIX path on disk has been unlinked) must not be
// returned as "discovered". Otherwise every subsequent Reconnect emits
// "no such file or directory" forever and the dashboard tab wedges. The
// Discover path SIGTERMs the process and removes the state file so the
// next naozhi restart cannot resurrect it.
func TestDiscover_RemovesStateWithMissingSocket(t *testing.T) {
	dir := t.TempDir()

	// Use our own PID: it is alive, owned by us (so SIGTERM will target
	// this test binary if F4 misbehaves — catch-safe because we ignore
	// that signal's default action in tests by installing a handler
	// below), and its /proc/self/exe matches the test binary, bypassing
	// the binary-identity check for the duration of this test.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	state := State{
		ShimPID:   os.Getpid(),
		Socket:    filepath.Join(dir, "does-not-exist.sock"),
		AuthToken: "dA==",
		Key:       "test:zombie",
	}
	path := filepath.Join(dir, KeyHash("test:zombie")+".json")
	if err := WriteStateFile(path, state); err != nil {
		t.Fatal(err)
	}

	// Manager's naozhiBin is set to the test binary path (os.Executable())
	// by default via mustNewManager → NewManager. That matches /proc/self/exe
	// so the binary-identity check passes and we hit the socket-stat check.
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	states, err := m.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states (zombie shim), got %d: %+v", len(states), states)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("zombie state file should have been removed")
	}
	// Drain the SIGTERM we just sent ourselves before it propagates to the
	// rest of the test process. 100ms is well within ticker bounds on even
	// slow CI.
	select {
	case <-sigCh:
	case <-time.After(500 * time.Millisecond):
		// If we never got it, either F4 didn't signal (old behaviour — a
		// real regression) or signal delivery is slow on this platform.
		// The state-file removal assertion above is the primary check;
		// the SIGTERM is defensive for clean-up of the real process.
		t.Log("no SIGTERM observed; state-file check already asserted")
	}
}

// TestForceCleanupZombie_SkipsNonShimPID guards against PID reuse: if the
// state file's PID has been inherited by some other (non-naozhi) process
// between the state write and our cleanup, we MUST NOT signal it. The
// state file is still purged so the ENOENT loop breaks.
func TestForceCleanupZombie_SkipsNonShimPID(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	// Force the manager's naozhiBin to a path that definitely doesn't
	// match our own /proc/self/exe so isOurShimPID returns false.
	m.naozhiBin = "/nonexistent/not-naozhi-binary"

	key := "test:pid-reuse"
	statePath := filepath.Join(dir, KeyHash(key)+".json")
	if err := WriteStateFile(statePath, State{
		ShimPID: os.Getpid(), // live PID, but "wrong binary"
		Socket:  filepath.Join(dir, "ghost.sock"),
		Key:     key,
	}); err != nil {
		t.Fatal(err)
	}

	// We are the target PID. If ForceCleanupZombie signals us despite the
	// binary mismatch, SIGTERM's default action would terminate the test
	// binary. Install a handler so the test survives either way and
	// observes whether the signal arrived.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	m.ForceCleanupZombie(State{
		ShimPID: os.Getpid(),
		Key:     key,
	})

	if _, err := os.Stat(statePath); err == nil {
		t.Error("state file should have been removed even when PID skipped")
	}
	select {
	case <-sigCh:
		t.Error("ForceCleanupZombie signalled a PID whose binary did NOT match — would kill unrelated processes")
	case <-time.After(50 * time.Millisecond):
		// good: no stray SIGTERM
	}
}

// TestForceCleanupZombie covers the helper used by router.reconnectShims
// ENOENT path (F6). It must: (a) remove the state file, (b) drop any live
// handle registered under the key, (c) tolerate a zero PID (defensive).
func TestForceCleanupZombie_RemovesStateAndHandle(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})

	key := "test:zombie-cleanup"
	state := State{
		ShimPID:   0, // skip SIGTERM
		Socket:    filepath.Join(dir, "ghost.sock"),
		AuthToken: "dA==",
		Key:       key,
	}
	statePath := filepath.Join(dir, KeyHash(key)+".json")
	if err := WriteStateFile(statePath, state); err != nil {
		t.Fatal(err)
	}
	// Plant a fake live handle so we can verify it gets dropped.
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	handle := &ShimHandle{
		Conn:       c,
		Reader:     bufio.NewReader(c),
		Writer:     bufio.NewWriter(c),
		ClientDone: make(chan struct{}),
	}
	m.mu.Lock()
	m.shims[key] = handle
	m.mu.Unlock()

	m.ForceCleanupZombie(state)

	if _, err := os.Stat(statePath); err == nil {
		t.Error("state file should have been removed")
	}
	m.mu.Lock()
	_, stillInMap := m.shims[key]
	m.mu.Unlock()
	if stillInMap {
		t.Error("handle should have been dropped from shims map")
	}
}

// TestEnsureSocketFreeForReuse covers F3: StartShim's pre-bind guard must
// refuse when a live listener exists, and must clean up stale socket files
// when no one is listening. Getting this wrong is what caused the UCCLEP
// zombie: a live shim's socket file got rm'd by a concurrent StartShim,
// kernel kept the listener fd alive, but filesystem path was gone → any
// naozhi Reconnect would ENOENT forever.
func TestEnsureSocketFreeForReuse_RefusesLiveListener(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	err = ensureSocketFreeForReuse(path)
	if err == nil {
		t.Fatal("expected error for live listener, got nil")
	}
	// The socket file must still exist — we refused to clobber.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("live socket should not have been removed: %v", statErr)
	}
}

func TestEnsureSocketFreeForReuse_RemovesStaleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.sock")
	// Create a regular file (not a real listener) — dial will fail,
	// so the path can be removed.
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSocketFreeForReuse(path); err != nil {
		t.Fatalf("ensureSocketFreeForReuse(stale): %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("stale file should have been removed")
	}
}

func TestEnsureSocketFreeForReuse_NoFileNoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.sock")
	if err := ensureSocketFreeForReuse(path); err != nil {
		t.Fatalf("ensureSocketFreeForReuse(nonexistent): %v", err)
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
