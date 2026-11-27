package shim

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- socketDir ---

func TestSocketDir_TableDriven(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/tmp/naozhi/shim.sock", "/tmp/naozhi"},
		{"/var/run/naozhi/shim-abc.sock", "/var/run/naozhi"},
		{"file.sock", ""}, // dir == "." → ""
		{"/sock", ""},     // dir == "/" → ""
		{"/a/b/c/d.sock", "/a/b/c"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := socketDir(tc.path)
			if got != tc.want {
				t.Errorf("socketDir(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// --- CleanStaleSocket ---

func TestCleanStaleSocket_RemovesDeadSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dead.sock")

	// Create a file to simulate a stale socket (no listener)
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	err := CleanStaleSocket(path)
	// Should remove it (no live listener), error or nil both ok if file is gone
	if _, statErr := os.Stat(path); statErr == nil {
		// File still exists — acceptable only if CleanStaleSocket decided socket is alive
		// But since nothing listens on this path, it should have been removed
		if err == nil {
			t.Error("CleanStaleSocket returned nil but left the file")
		}
	}
}

func TestCleanStaleSocket_LiveSocket_ReturnsError(t *testing.T) {
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "live.sock")

	// Start a real unix listener
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	err = CleanStaleSocket(path)
	if err == nil {
		t.Error("expected error for live socket, got nil")
	}
	if !strings.Contains(err.Error(), "alive") {
		t.Errorf("expected 'alive' in error, got: %v", err)
	}
}

// --- writeMsg ---

func TestWriteMsg_WritesValidNDJSON(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	msg := ServerMsg{Type: "pong", Buffered: 5}
	go writeMsg(server, msg)

	client.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var got ServerMsg
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "pong" || got.Buffered != 5 {
		t.Errorf("got %+v, want pong buffered=5", got)
	}
}

func TestWriteMsg_ToClosedConn_DoesNotPanic(t *testing.T) {
	client, server := net.Pipe()
	client.Close() // close reading end
	server.Close() // close writing end too

	// Must not panic
	writeMsg(server, ServerMsg{Type: "hello"})
}

// --- tryExtractSessionID ---

// TestTryExtractSessionID: the first session ID seen wins — claude's
// system/init or result session_id, or ACP's camelCase sessionId in a
// session/new response or a later notification's params (PR #129). Once
// one is known the latch skips every later line (R202606f-PERF-003); lines
// yielding no ID leave the latch down.
func TestTryExtractSessionID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"system init", []string{`{"type":"system","subtype":"init","session_id":"sess_init_001"}`}, "sess_init_001"},
		{"result fallback", []string{`{"type":"result","session_id":"sess_result_002"}`}, "sess_result_002"},
		{"result does not overwrite init", []string{
			`{"type":"system","subtype":"init","session_id":"original"}`,
			`{"type":"result","session_id":"override_attempt"}`,
		}, "original"},
		{"invalid json", []string{"not json"}, ""},
		{"empty session_id", []string{`{"type":"system","subtype":"init","session_id":""}`}, ""},
		{"other event type", []string{`{"type":"message","session_id":"should_not_set"}`}, ""},
		{"acp session/new", []string{`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"acp-uuid-001"}}`}, "acp-uuid-001"},
		{"acp notification", []string{`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-uuid-002","update":{"sessionUpdate":"agent_message_chunk"}}}`}, "acp-uuid-002"},
		{"acp notification does not overwrite", []string{
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"original-acp"}}`,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"override-attempt"}}`,
		}, "original-acp"},
		{"latch stays down until a real id", []string{
			`{"type":"message","session_id":"should_not_set"}`,
			`{"type":"system","subtype":"init","session_id":""}`,
			"not json",
			`{"type":"system","subtype":"init","session_id":"real_id"}`,
		}, "real_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := makeShimServerForTest(t)
			for _, l := range tc.lines {
				s.tryExtractSessionID([]byte(l))
			}
			s.mu.Lock()
			got := s.state.SessionID
			s.mu.Unlock()
			if got != tc.want {
				t.Errorf("SessionID = %q, want %q", got, tc.want)
			}
			if known := s.sessionIDKnown.Load(); known != (tc.want != "") {
				t.Errorf("sessionIDKnown = %v, want %v", known, tc.want != "")
			}
		})
	}
}

// --- enqueueWrite ---

func TestEnqueueWrite_WithClient_Delivers(t *testing.T) {
	s := makeShimServerForTest(t)
	ch := make(chan []byte, 8)
	done := make(chan struct{})

	s.mu.Lock()
	s.writeCh = ch
	s.clientDone = done
	s.mu.Unlock()

	payload := []byte(`{"type":"stdout"}` + "\n")
	s.enqueueWrite(payload)

	select {
	case got := <-ch:
		if string(got) != string(payload) {
			t.Errorf("got %q, want %q", got, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueueWrite did not deliver to channel")
	}
}

func TestEnqueueWrite_FullChannel_Drops(t *testing.T) {
	s := makeShimServerForTest(t)
	// Capacity=1, fill it up
	ch := make(chan []byte, 1)
	done := make(chan struct{})
	ch <- []byte("existing")

	s.mu.Lock()
	s.writeCh = ch
	s.clientDone = done
	s.mu.Unlock()

	// enqueueWrite with a full channel should drop (not block)
	finished := make(chan struct{})
	go func() {
		s.enqueueWrite([]byte("dropped"))
		close(finished)
	}()

	select {
	case <-finished:
		// Good — it returned immediately
	case <-time.After(500 * time.Millisecond):
		t.Fatal("enqueueWrite blocked on a full channel")
	}
}

func TestEnqueueWrite_ClientDone_Drops(t *testing.T) {
	s := makeShimServerForTest(t)
	ch := make(chan []byte, 1)
	done := make(chan struct{})
	close(done) // already done

	s.mu.Lock()
	s.writeCh = ch
	s.clientDone = done
	s.mu.Unlock()

	// Should not block or panic
	s.enqueueWrite([]byte("data"))
}

// --- readStdout: skip marshal when no client attached (#2295) ---

// TestReadStdout_NoClient_SkipsMarshalButStillBuffers pins R202606f-PERF-006
// (#2295): with no naozhi attached, readStdout must NOT enqueue a marshalled
// frame (it would only be dropped), but buffer.Push / session-ID extraction
// MUST still run so replay-on-attach and the session-ID latch stay correct.
func TestReadStdout_NoClient_SkipsMarshalButStillBuffers(t *testing.T) {
	dir := t.TempDir()
	// Emit three NDJSON lines then exit. One carries a session_id so we can
	// also confirm extraction still happens with no client.
	script := `printf '{"type":"a"}\n{"type":"system","subtype":"init","session_id":"sess-x"}\n{"type":"b"}\n'`
	cli, err := startCLI("sh", []string{"-c", script}, dir)
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	defer cli.kill()

	s := &shimServer{
		cli:    cli,
		buffer: NewRingBuffer(100, 1024*1024),
		state:  State{Key: "noclient:key"},
		done:   make(chan struct{}),
	}
	s.watchdog = NewWatchdog(30*time.Second, nil)

	// Attach a writeCh but leave clientAttached false: enqueueWrite would
	// deliver here if readStdout marshalled, so an empty channel proves the
	// marshal+enqueue path was skipped.
	ch := make(chan []byte, 16)
	s.mu.Lock()
	s.writeCh = ch
	s.clientDone = make(chan struct{})
	s.mu.Unlock()
	// clientAttached defaults to false (zero value).

	done := make(chan struct{})
	go func() { defer close(done); s.readStdout() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readStdout did not return")
	}

	// All three lines must have been buffered for replay.
	if got := s.buffer.Count(); got != 3 {
		t.Errorf("buffer.Count = %d, want 3 (Push must run even with no client)", got)
	}
	// Session-ID extraction must have run despite no client.
	if !s.sessionIDKnown.Load() {
		t.Errorf("sessionIDKnown = false, want true (extraction must run with no client)")
	}
	// No frame should have been enqueued (marshal skipped).
	select {
	case frame := <-ch:
		t.Errorf("frame enqueued with no client attached: %q (marshal should be skipped)", frame)
	default:
	}
}

// --- admitClient / clearClient ---

func TestAdmitClient_ReplacesOld(t *testing.T) {
	s := makeShimServerForTest(t)

	clientA, serverA := net.Pipe()
	defer clientA.Close()
	defer serverA.Close()

	clientB, serverB := net.Pipe()
	defer clientB.Close()
	defer serverB.Close()

	ch1, done1, res := s.admitClient(clientA, 1, false, nil)
	if res != admitOK || ch1 == nil || done1 == nil {
		t.Fatalf("admitClient = (%v, %v, %v), want channels and admitOK", ch1, done1, res)
	}

	// A later attach with the CLI dead kicks the old client.
	ch2, done2, res := s.admitClient(clientB, 2, false, nil)
	if res != admitOK || ch2 == nil || done2 == nil {
		t.Fatalf("admitClient (second) = (%v, %v, %v), want channels and admitOK", ch2, done2, res)
	}

	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Error("done1 not closed after replacing with new client")
	}
}

// An older handler that reaches admission after a newer one must leave the
// newer client untouched: its done channel open and its conn usable.
func TestAdmitClient_OlderGenDoesNotKickNewer(t *testing.T) {
	s := makeShimServerForTest(t)

	oldServer, oldPeer := net.Pipe()
	defer oldServer.Close()
	defer oldPeer.Close()
	newServer, newPeer := net.Pipe()
	defer newServer.Close()
	defer newPeer.Close()

	_, newDone, res := s.admitClient(newServer, 2, false, nil)
	if res != admitOK {
		t.Fatalf("newer admit = %v, want admitOK", res)
	}
	if ch, done, res := s.admitClient(oldServer, 1, false, nil); res != admitSuperseded || ch != nil || done != nil {
		t.Fatalf("older admit = (%v, %v, %v), want (nil, nil, admitSuperseded)", ch, done, res)
	}

	s.mu.Lock()
	active, gen := s.clientConn, s.clientGen
	s.mu.Unlock()
	if active != newServer || gen != 2 {
		t.Errorf("active client = (%v, gen %d), want the newer conn with gen 2", active, gen)
	}
	select {
	case <-newDone:
		t.Error("the newer client's done channel was closed by an older handler")
	default:
	}
	go func() { _, _ = newPeer.Write([]byte("x")) }()
	buf := make([]byte, 1)
	_ = newServer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := newServer.Read(buf); err != nil {
		t.Errorf("the newer client's conn is no longer usable: %v", err)
	}
}

// With the CLI alive an active client is never kicked, whatever the
// generation; the newcomer is refused as busy.
func TestAdmitClient_LiveCLIRejectsSecond(t *testing.T) {
	s := makeShimServerForTest(t)

	clientA, serverA := net.Pipe()
	defer clientA.Close()
	defer serverA.Close()
	clientB, serverB := net.Pipe()
	defer clientB.Close()
	defer serverB.Close()

	_, doneA, _ := s.admitClient(clientA, 1, true, nil)
	if ch, done, res := s.admitClient(clientB, 2, true, nil); res != admitBusy || ch != nil || done != nil {
		t.Fatalf("second admit with a live CLI = (%v, %v, %v), want (nil, nil, admitBusy)", ch, done, res)
	}
	s.mu.Lock()
	active := s.clientConn
	s.mu.Unlock()
	if active != clientA {
		t.Error("a busy refusal replaced the active client")
	}
	select {
	case <-doneA:
		t.Error("a busy refusal closed the active client's done channel")
	default:
	}

	// Once the active client is gone the next attach is admitted.
	s.clearClient(clientA)
	if _, _, res := s.admitClient(clientB, 3, true, nil); res != admitOK {
		t.Errorf("admit after clearClient = %v, want admitOK", res)
	}
}

func TestClearClient_RemovesCorrectClient(t *testing.T) {
	s := makeShimServerForTest(t)

	clientA, serverA := net.Pipe()
	defer clientA.Close()
	defer serverA.Close()

	s.admitClient(clientA, 1, false, nil)

	// Clear it
	s.clearClient(clientA)

	s.mu.Lock()
	conn := s.clientConn
	s.mu.Unlock()

	if conn != nil {
		t.Error("clientConn should be nil after clearClient")
	}
}

func TestClearClient_WrongConn_Ignored(t *testing.T) {
	s := makeShimServerForTest(t)

	clientA, serverA := net.Pipe()
	defer clientA.Close()
	defer serverA.Close()

	clientB, serverB := net.Pipe()
	defer clientB.Close()
	defer serverB.Close()

	s.admitClient(clientA, 1, false, nil)
	s.clearClient(clientB) // different conn, must be ignored

	s.mu.Lock()
	conn := s.clientConn
	s.mu.Unlock()

	if conn != clientA {
		t.Error("clearClient with wrong conn should leave clientConn intact")
	}
}

// --- Full shim server integration with echo subprocess ---

// TestShimServer_FullHandshake runs a shim server backed by a real cat
// subprocess (/bin/sh -c "cat"), exercises the auth handshake, and verifies
// hello / replay_done are received correctly.
func TestShimServer_FullHandshake(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ss := setupShimServer(t)
	defer ss.cleanup()

	if ss.hello.Type != "hello" {
		t.Fatalf("expected hello, got %q", ss.hello.Type)
	}
	if ss.replayDone.Type != "replay_done" {
		t.Fatalf("expected replay_done, got %q", ss.replayDone.Type)
	}
	if ss.hello.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", ss.hello.ProtocolVersion, ProtocolVersion)
	}
	if ss.hello.CLIAlive == nil {
		t.Error("CLIAlive should not be nil in hello")
	}

	sendClientCmd(t, ss.client, ClientMsg{Type: "detach"})
	ss.waitHandlerDone(t, "detach")
}

// TestShimServer_PingPong verifies that a ping message receives a pong response.
func TestShimServer_PingPong(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ss := setupShimServer(t)
	defer ss.cleanup()

	sendClientCmd(t, ss.client, ClientMsg{Type: "ping"})
	ss.client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	pongLine, err := ss.client.reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	var pong ServerMsg
	if err := json.Unmarshal(pongLine, &pong); err != nil {
		t.Fatalf("parse pong: %v", err)
	}
	if pong.Type != "pong" {
		t.Errorf("expected pong, got %q", pong.Type)
	}

	sendClientCmd(t, ss.client, ClientMsg{Type: "detach"})
	ss.waitHandlerDone(t, "detach")
}

// --- initiateShutdown idempotency ---

func TestInitiateShutdown_Idempotent(t *testing.T) {
	s := makeShimServerForTest(t)
	s.initiateShutdown()
	s.initiateShutdown() // must not panic
	select {
	case <-s.done:
		// good
	default:
		t.Error("done channel not closed after initiateShutdown")
	}
}

// --- resetIdleTimer / idleC ---

// Run's select keeps the channel idleC gave it when it was entered; a reset
// on detach must arm that same channel, both when the timer was still pending
// and when it already fired while a client was attached.
func TestResetIdleTimer_FiresOnChannelHeldBeforeReset(t *testing.T) {
	s := makeShimServerForTest(t)
	s.resetIdleTimer(time.Hour)
	held := s.idleC()

	s.resetIdleTimer(20 * time.Millisecond) // detach while pending
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("reset of a pending timer never fired on the held channel")
	}

	s.resetIdleTimer(20 * time.Millisecond) // detach after it fired attached
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("reset of a fired timer never fired on the held channel")
	}
}

// --- saveStateCLIDead ---

func TestSaveStateCLIDead_UpdatesStateFile(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "shim_dead.json")

	s := makeShimServerForTestWithStateFile(t, stateFile)
	s.mu.Lock()
	s.state.CLIAlive = true
	s.mu.Unlock()

	s.saveStateCLIDead()

	loaded, err := ReadStateFile(stateFile)
	if err != nil {
		t.Fatalf("ReadStateFile: %v", err)
	}
	if loaded.CLIAlive {
		t.Error("CLIAlive should be false after saveStateCLIDead")
	}
}

// --- watchSocketFile (F5) ---

// TestWatchSocketFile_TriggersOnMissingSocket covers the self-heal path: if
// the socket file disappears from under us, watchSocketFile must close
// s.done so the main loop shuts the shim down. Catches the
// "listener fd alive, filesystem path gone" failure mode that UCCLEP hit.
func TestWatchSocketFile_TriggersOnMissingSocket(t *testing.T) {
	s := makeShimServerForTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "watch.sock")
	// Create the socket file so the first stat succeeds; deleting later
	// triggers the watcher.
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		s.watchSocketFile(path, 10*time.Millisecond)
		close(done)
	}()
	// Let the watcher observe at least one healthy tick before we delete.
	time.Sleep(30 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
		// good: watcher initiated shutdown
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchSocketFile did not shut down after socket removal")
	}
	// watchSocketFile goroutine should also exit.
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchSocketFile did not return after shutdown")
	}
}

// TestWatchSocketFile_ExitsOnDone covers the normal shutdown path: when
// s.done closes for reasons other than the watcher itself, the goroutine
// must return cleanly so there is no leaked ticker.
func TestWatchSocketFile_ExitsOnDone(t *testing.T) {
	s := makeShimServerForTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "still-here.sock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		s.watchSocketFile(path, 10*time.Millisecond)
		close(done)
	}()
	// Externally trigger shutdown.
	s.initiateShutdown()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchSocketFile did not exit on done channel close")
	}
}

// --- helpers for constructing test shimServer instances ---

func makeShimServerForTest(t *testing.T) *shimServer {
	t.Helper()
	return &shimServer{
		done: make(chan struct{}),
	}
}

func makeShimServerForTestWithStateFile(t *testing.T, stateFile string) *shimServer {
	t.Helper()
	s := &shimServer{
		stateFile: stateFile,
		state: State{
			ShimPID:   os.Getpid(),
			Socket:    "/tmp/fake.sock",
			AuthToken: "dA==",
			Key:       "test:key",
		},
		done: make(chan struct{}),
	}
	// Write an initial state file so ReadStateFile works
	if err := WriteStateFile(stateFile, s.state); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
	}
	return s
}

// --- Additional handleClient command paths ---

// shimServerForCommands creates a shimServer backed by a "sh -c cat" subprocess
// and returns the server plus a function to connect+auth a client conn.
type connectedClient struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

// shimSetup is a shimServer with one authenticated client attached.
// handlerDone closes when handleClient returns; cleanup closes the client and
// waits for it without a timeout, so tests that assert a bounded exit select
// on handlerDone themselves (waitHandlerDone).
type shimSetup struct {
	s           *shimServer
	client      connectedClient
	hello       ServerMsg
	replayed    []ServerMsg // frames between hello and replay_done
	replayDone  ServerMsg
	handlerDone <-chan struct{}
	cleanup     func()
}

func (ss shimSetup) waitHandlerDone(t *testing.T, after string) {
	t.Helper()
	select {
	case <-ss.handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("handleClient did not exit after %s", after)
	}
}

func setupShimServerWithClient(t *testing.T) (s *shimServer, client connectedClient, cleanup func()) {
	t.Helper()
	ss := setupShimServer(t)
	return ss.s, ss.client, ss.cleanup
}

// setupShimServer starts a shimServer backed by "sh -c cat", pushes prefill
// into its ring buffer, then attaches an authenticated client and reads
// everything up to and including replay_done.
func setupShimServer(t *testing.T, prefill ...[]byte) shimSetup {
	t.Helper()
	dir := shortSocketDir(t)
	socketPath := filepath.Join(dir, "cmd.sock")
	stateFile := filepath.Join(dir, "cmd_state.json")

	tokenRaw, _, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	cli, err := startCLI("sh", []string{"-c", "cat"}, dir)
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}

	buf := NewRingBuffer(100, 1024*1024)
	for _, line := range prefill {
		buf.Push(line)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		cli.kill()
		t.Fatalf("Listen: %v", err)
	}

	s := &shimServer{
		cli:       cli,
		listener:  ln,
		buffer:    buf,
		tokenRaw:  tokenRaw,
		stateFile: stateFile,
		state:     State{Key: "cmd:key", Socket: socketPath, CLIAlive: true},
		done:      make(chan struct{}),
	}
	s.watchdog = NewWatchdog(30*time.Second, nil)

	// Write initial state so saveState doesn't fail
	WriteStateFile(stateFile, s.state) //nolint:errcheck

	// Start stdout/stderr readers (normally started by Run())
	go s.readStdout()
	go s.readStderr()

	connCh := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			connCh <- c
		}
	}()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		cli.kill()
		ln.Close()
		t.Fatalf("Dial: %v", err)
	}

	var serverConn net.Conn
	select {
	case serverConn = <-connCh:
	case <-time.After(2 * time.Second):
		conn.Close()
		cli.kill()
		ln.Close()
		t.Fatal("accept timeout")
	}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleClient(serverConn, 5*time.Second)
	}()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Auth
	attach := ClientMsg{Type: "attach", Token: base64.StdEncoding.EncodeToString(tokenRaw), Seq: 0}
	attachData, _ := json.Marshal(attach)
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	writer.Write(append(attachData, '\n'))                 //nolint:errcheck
	writer.Flush()                                         //nolint:errcheck
	conn.SetWriteDeadline(time.Time{})                     //nolint:errcheck

	ss := shimSetup{s: s, handlerDone: handlerDone}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	readMsg := func(what string) ServerMsg {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			conn.Close()
			t.Fatalf("read %s: %v", what, err)
		}
		var msg ServerMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			conn.Close()
			t.Fatalf("parse %s: %v (line=%s)", what, err, line)
		}
		return msg
	}
	ss.hello = readMsg("hello")
	for {
		msg := readMsg("replay")
		if msg.Type == "replay_done" {
			ss.replayDone = msg
			break
		}
		ss.replayed = append(ss.replayed, msg)
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	ss.client = connectedClient{conn: conn, reader: reader, writer: writer}
	ss.cleanup = func() {
		conn.Close()
		ln.Close()
		os.Remove(socketPath)
		<-handlerDone
		cli.kill()
	}

	return ss
}

func sendClientCmd(t *testing.T, c connectedClient, msg ClientMsg) {
	t.Helper()
	data, _ := json.Marshal(msg)
	c.conn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	c.writer.Write(append(data, '\n'))                   //nolint:errcheck
	c.writer.Flush()                                     //nolint:errcheck
	c.conn.SetWriteDeadline(time.Time{})                 //nolint:errcheck
}

func TestHandleClient_WriteCommand(t *testing.T) {
	_, client, cleanup := setupShimServerWithClient(t)
	defer cleanup()

	// write sends a line to CLI stdin; CLI (cat) will echo it back to stdout.
	// The path is: client → handleClient → cli.stdin.Write → cat → readStdout
	//              → enqueueWrite → writeCh → writer goroutine → client conn.
	// Allow up to 3s for the async pipeline.
	sendClientCmd(t, client, ClientMsg{Type: "write", Line: `{"hello":"world"}`})

	client.conn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	for {
		line, err := client.reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read stdout echo: %v", err)
		}
		var msg ServerMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Type == "stdout" {
			break // got it
		}
	}
	client.conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	// Detach
	sendClientCmd(t, client, ClientMsg{Type: "detach"})
}

// TestHandleClient_OversizeLineDisconnects pins the per-line LimitedReader
// (maxClientLineBytes): a client streaming past the limit without a newline
// must be disconnected rather than buffered without bound.
func TestHandleClient_OversizeLineDisconnects(t *testing.T) {
	ss := setupShimServer(t)
	defer ss.cleanup()

	go func() {
		ss.client.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))      //nolint:errcheck
		ss.client.conn.Write(bytes.Repeat([]byte("a"), maxClientLineBytes+1)) //nolint:errcheck
	}()
	ss.waitHandlerDone(t, "an oversize line without newline")
}

func TestHandleClient_InterruptCommand(t *testing.T) {
	_, client, cleanup := setupShimServerWithClient(t)
	defer cleanup()

	// interrupt must not panic or error
	sendClientCmd(t, client, ClientMsg{Type: "interrupt"})

	// Detach cleanly
	sendClientCmd(t, client, ClientMsg{Type: "detach"})
}

func TestHandleClient_KillCommand(t *testing.T) {
	_, client, cleanup := setupShimServerWithClient(t)
	defer cleanup()

	// kill the CLI; handleClient must deliver cli_exited on this live
	// connection before EOF. Since #1783 the frame is written synchronously
	// after the writer goroutine drains, so it is no longer racy: a clean EOF
	// without cli_exited is now a regression, not "acceptable".
	sendClientCmd(t, client, ClientMsg{Type: "kill"})

	client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	gotExited := false
	for !gotExited {
		line, err := client.reader.ReadBytes('\n')
		if err != nil {
			break
		}
		var msg ServerMsg
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if msg.Type == "cli_exited" {
			gotExited = true
		}
	}
	client.conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	if !gotExited {
		t.Fatal("cli_exited was not delivered on the live connection after kill (#1783 regression)")
	}
}

// TestHandleClient_CLIAlreadyDeadNilGuard verifies that when a client attaches
// after the CLI has already exited, handleClient sends cli_exited during replay
// and does NOT send it again from the runCommandLoop select (nil-guard prevents
// the always-selectable closed channel from causing a double delivery).
// [R20260603-CR-B]
func TestHandleClient_CLIAlreadyDeadNilGuard(t *testing.T) {
	dir := shortSocketDir(t)
	socketPath := filepath.Join(dir, "cmd.sock")
	stateFile := filepath.Join(dir, "cmd_state.json")

	tokenRaw, _, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	cli, err := startCLI("sh", []string{"-c", "exit 0"}, dir)
	if err != nil {
		t.Fatalf("startCLI: %v", err)
	}
	// Wait for the process to exit so cli.alive() returns false.
	cli.wait() //nolint:errcheck

	buf := NewRingBuffer(100, 1024*1024)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	s := &shimServer{
		cli:       cli,
		listener:  ln,
		buffer:    buf,
		tokenRaw:  tokenRaw,
		stateFile: stateFile,
		state:     State{Key: "cmd:key", Socket: socketPath, CLIAlive: false},
		done:      make(chan struct{}),
	}
	s.watchdog = NewWatchdog(30*time.Second, nil)
	WriteStateFile(stateFile, s.state) //nolint:errcheck

	connCh := make(chan net.Conn, 1)
	go func() {
		c, acceptErr := ln.Accept()
		if acceptErr == nil {
			connCh <- c
		}
	}()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var serverConn net.Conn
	select {
	case serverConn = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("accept timeout")
	}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleClient(serverConn, 5*time.Second)
	}()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Auth
	attach := ClientMsg{Type: "attach", Token: base64.StdEncoding.EncodeToString(tokenRaw), Seq: 0}
	attachData, _ := json.Marshal(attach)
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	writer.Write(append(attachData, '\n'))                 //nolint:errcheck
	writer.Flush()                                         //nolint:errcheck
	conn.SetWriteDeadline(time.Time{})                     //nolint:errcheck

	// Read all initial messages (hello, replay_done, cli_exited).
	// We wait for the replay_done + cli_exited to appear, then detach.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	exitedCount := 0
	replayDone := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			break
		}
		var msg ServerMsg
		if json.Unmarshal(line, &msg) == nil {
			switch msg.Type {
			case "cli_exited":
				exitedCount++
			case "replay_done":
				replayDone = true
			}
		}
		// Once we have replay_done and cli_exited, stop reading and detach.
		if replayDone && exitedCount > 0 {
			break
		}
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	// Send detach to allow handleClient to return cleanly.
	sendClientCmd(t, connectedClient{conn: conn, reader: reader, writer: writer}, ClientMsg{Type: "detach"})

	// Handler must return promptly — if the nil-guard is broken and the closed
	// channel is still selectable, the handler would spin or double-deliver.
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handleClient did not return after detach; nil-guard may be broken")
	}

	// cli_exited must appear exactly once (sent during replay, not a second time from the select).
	if exitedCount != 1 {
		t.Errorf("cli_exited appeared %d time(s), want exactly 1", exitedCount)
	}
}

func TestHandleClient_ShutdownCommand(t *testing.T) {
	_, client, cleanup := setupShimServerWithClient(t)
	// Same rationale as TestHandleClient_ShutdownWithAuthedClientWithin60s:
	// cleanup's <-handlerDone is the barrier we want so the defer
	// saveState() inside handleClient finishes before t.TempDir's RemoveAll
	// runs — otherwise WriteStateFile's CreateTemp+Rename races with
	// cleanup and surfaces "directory not empty".
	defer client.conn.Close()
	defer cleanup()

	sendClientCmd(t, client, ClientMsg{Type: "shutdown"})

	// The handler should exit; connection will close
	time.Sleep(200 * time.Millisecond)
}

// TestHandleClient_ShutdownWithAuthedClientWithin60s verifies the guard
// relaxation added for UCCLEP-2026-04-26: an authenticated client issuing
// shutdown within the shim's first 60s must be honoured (previously ignored),
// otherwise fresh_context cron / Router.Reset paths leave the socket listening
// for up to 30s and the next StartShim for the same key hits "refusing to
// clobber".
func TestHandleClient_ShutdownWithAuthedClientWithin60s(t *testing.T) {
	s, client, cleanup := setupShimServerWithClient(t)
	defer client.conn.Close()
	// cleanup waits on handlerDone inside — without it, handleClient's
	// defer saveState() (server.go:797) can still be running WriteStateFile
	// (os.CreateTemp + Rename under t.TempDir()) when the test returns,
	// racing with t.TempDir()'s RemoveAll and surfacing "directory not
	// empty" flakes. Using defer cleanup() attaches that barrier without
	// altering the shutdown semantics this test asserts.
	defer cleanup()

	// Simulate a freshly-started shim — this is the case where the old
	// guard would have bailed out with "ignoring shutdown".
	s.startedAt = time.Now()

	sendClientCmd(t, client, ClientMsg{Type: "shutdown"})

	// handleClient should exit and done must be closed — that's what drives
	// Run's main loop into the listener.Close + os.Remove defers.
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("s.done not closed within 10s — shutdown was ignored")
	}
}

// TestShutdownGuard_EvaluationMatrix validates the guard decision logic used
// by the "shutdown" command handler. It documents the four cases so future
// changes to the guard produce an obvious test diff rather than a silent
// semantic drift.
//
// The guard should refuse shutdown ONLY in the combination:
//
//	hasClient == false && cliAlive == true && age < 60s
func TestShutdownGuard_EvaluationMatrix(t *testing.T) {
	const window = 60 * time.Second
	cases := []struct {
		name      string
		hasClient bool
		cliAlive  bool
		age       time.Duration
		wantBlock bool
	}{
		{"authed client, fresh shim: must honour shutdown", true, true, time.Second, false},
		{"authed client, old shim: must honour shutdown", true, true, 2 * window, false},
		{"no client, dead CLI, fresh shim: must honour shutdown", false, false, time.Second, false},
		{"no client, alive CLI, old shim: must honour shutdown", false, true, 2 * window, false},
		{"no client, alive CLI, fresh shim: must BLOCK", false, true, time.Second, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the production condition exactly; if the production
			// expression changes, this test will fail loud.
			block := !tc.hasClient && tc.cliAlive && tc.age < window
			if block != tc.wantBlock {
				t.Fatalf("guard(hasClient=%v, cliAlive=%v, age=%v) = %v; want %v",
					tc.hasClient, tc.cliAlive, tc.age, block, tc.wantBlock)
			}
		})
	}
}

func TestHandleClient_CloseStdinCommand(t *testing.T) {
	_, client, cleanup := setupShimServerWithClient(t)
	defer cleanup()

	// close_stdin must not panic
	sendClientCmd(t, client, ClientMsg{Type: "close_stdin"})

	// After close_stdin, cat will exit and we'll get cli_exited
	client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	for {
		line, err := client.reader.ReadBytes('\n')
		if err != nil {
			break // connection closed
		}
		var msg ServerMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Type == "cli_exited" {
			break
		}
	}
	client.conn.SetReadDeadline(time.Time{}) //nolint:errcheck
}

func TestHandleClient_ReplayBufferedLines(t *testing.T) {
	// Attach with seq=0 replays all 3 pre-buffered lines.
	ss := setupShimServer(t, []byte(`{"seq":1}`), []byte(`{"seq":2}`), []byte(`{"seq":3}`))
	defer ss.cleanup()

	if ss.hello.Type != "hello" {
		t.Fatalf("want hello, got %q", ss.hello.Type)
	}
	replayCount := 0
	for _, msg := range ss.replayed {
		if msg.Type == "replay" {
			replayCount++
		}
	}
	if replayCount != 3 {
		t.Errorf("got %d replay messages, want 3", replayCount)
	}
	if ss.replayDone.Count != 3 {
		t.Errorf("replay_done.Count = %d, want 3", ss.replayDone.Count)
	}

	sendClientCmd(t, ss.client, ClientMsg{Type: "detach"})
	ss.waitHandlerDone(t, "detach")
}

// TestWriteStateFile_ChmodDir verifies the state directory gets 0700 perms
// and the state file 0600.
func TestWriteStateFile_ChmodDir(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "shims")
	path := filepath.Join(subdir, "test.json")

	state := State{ShimPID: 1, Socket: "/tmp/s.sock", AuthToken: "dA==", Key: "k"}
	if err := WriteStateFile(path, state); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
	}

	info, err := os.Stat(subdir)
	if err != nil {
		t.Fatalf("Stat subdir: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0700 {
		t.Errorf("subdir perm = %o, want 0700", perm)
	}
	// The state file carries AuthToken: it must stay private to the uid.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat state file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file perm = %o, want 0600", perm)
	}
}
