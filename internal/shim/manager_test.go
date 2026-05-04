package shim

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// newTestHandlePair creates a ShimHandle backed by one end of a net.Pipe(),
// and returns the other end for the "server side" to write to.
// net.Pipe supports deadlines and is suitable for testing.
func newTestHandlePair(t *testing.T) (handle *ShimHandle, server net.Conn) {
	t.Helper()
	client, srv := net.Pipe()
	handle = &ShimHandle{
		Conn:       client,
		Reader:     bufio.NewReader(client),
		Writer:     bufio.NewWriter(client),
		ClientDone: make(chan struct{}),
	}
	return handle, srv
}

// writeLine writes a NDJSON message + newline to a net.Conn, with a short deadline.
// Does NOT call t.Fatal so it is safe to call from goroutines.
func writeLine(_ *testing.T, conn net.Conn, msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	conn.Write(append(data, '\n'))                         //nolint:errcheck
	conn.SetWriteDeadline(time.Time{})                     //nolint:errcheck
}

// readLineMsgConn reads one NDJSON line from the conn with a deadline and parses it into dst.
// Returns error instead of calling t.Fatal so it is safe to call from goroutines.
func readLineMsgConn(conn net.Conn, dst interface{}) error {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	defer conn.SetReadDeadline(time.Time{})               //nolint:errcheck
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, dst)
}

// mustNewManager wraps NewManager + t.Fatal so tests do not have to repeat the
// error check. NewManager returns an error only when os.Executable() fails,
// which does not happen on any supported CI platform.
func mustNewManager(t *testing.T, cfg ManagerConfig) *Manager {
	t.Helper()
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// --- ShimHandle.SendMsg ---

func TestShimHandle_SendMsg(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	want := ClientMsg{Type: "write", Line: `{"type":"user"}`}

	// Start reader BEFORE SendMsg to avoid deadlock on net.Pipe
	gotCh := make(chan ClientMsg, 1)
	go func() {
		var msg ClientMsg
		readLineMsgConn(server, &msg)
		gotCh <- msg
	}()

	if err := handle.SendMsg(want); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	select {
	case got := <-gotCh:
		if got.Type != want.Type || got.Line != want.Line {
			t.Errorf("received %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message on server side")
	}
}

func TestShimHandle_SendMsg_MultipleTypes(t *testing.T) {
	tests := []struct {
		name string
		msg  ClientMsg
	}{
		{"attach", ClientMsg{Type: "attach", Token: "dGVzdA==", Seq: 5}},
		{"interrupt", ClientMsg{Type: "interrupt"}},
		{"ping", ClientMsg{Type: "ping"}},
		{"shutdown", ClientMsg{Type: "shutdown"}},
		{"detach", ClientMsg{Type: "detach"}},
		{"kill", ClientMsg{Type: "kill"}},
		{"close_stdin", ClientMsg{Type: "close_stdin"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handle, server := newTestHandlePair(t)
			defer handle.Conn.Close()
			defer server.Close()

			gotCh := make(chan ClientMsg, 1)
			go func() {
				var msg ClientMsg
				readLineMsgConn(server, &msg)
				gotCh <- msg
			}()

			if err := handle.SendMsg(tc.msg); err != nil {
				t.Fatalf("SendMsg(%s): %v", tc.name, err)
			}
			select {
			case got := <-gotCh:
				if got.Type != tc.msg.Type {
					t.Errorf("Type = %q, want %q", got.Type, tc.msg.Type)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timeout waiting for %s", tc.name)
			}
		})
	}
}

// --- ShimHandle.ReadMsg ---

func TestShimHandle_ReadMsg(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	want := ServerMsg{Type: "stdout", Seq: 99, Line: "some line"}

	// Write from server side
	go writeLine(t, server, want)

	handle.Conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	got, err := handle.ReadMsg()
	handle.Conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if got.Type != want.Type || got.Seq != want.Seq || got.Line != want.Line {
		t.Errorf("ReadMsg = %+v, want %+v", got, want)
	}
}

func TestShimHandle_ReadMsg_InvalidJSON(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	go func() {
		server.Write([]byte("not json\n")) //nolint:errcheck
	}()

	handle.Conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	_, err := handle.ReadMsg()
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestShimHandle_ReadMsg_ClosedConn(t *testing.T) {
	handle, server := newTestHandlePair(t)
	server.Close() // close the "server" end immediately

	handle.Conn.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	_, err := handle.ReadMsg()
	if err == nil {
		t.Error("expected error on closed connection, got nil")
	}
	handle.Conn.Close()
}

// --- ShimHandle.DrainReplay ---

func TestShimHandle_DrainReplay_Normal(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	// Server sends: 3 replay lines + replay_done
	go func() {
		for i := 1; i <= 3; i++ {
			writeLine(t, server, ServerMsg{Type: "replay", Seq: int64(i), Line: fmt.Sprintf("line%d", i)})
		}
		writeLine(t, server, ServerMsg{Type: "replay_done", Count: 3})
	}()

	replays, err := handle.DrainReplay()
	if err != nil {
		t.Fatalf("DrainReplay: %v", err)
	}
	if len(replays) != 3 {
		t.Fatalf("got %d replays, want 3", len(replays))
	}
	for i, r := range replays {
		if r.Type != "replay" || r.Seq != int64(i+1) {
			t.Errorf("replay[%d] = %+v, want type=replay seq=%d", i, r, i+1)
		}
	}
}

func TestShimHandle_DrainReplay_EmptyBuffer(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	go func() {
		writeLine(t, server, ServerMsg{Type: "replay_done", Count: 0})
	}()

	replays, err := handle.DrainReplay()
	if err != nil {
		t.Fatalf("DrainReplay: %v", err)
	}
	if len(replays) != 0 {
		t.Errorf("got %d replays, want 0", len(replays))
	}
}

func TestShimHandle_DrainReplay_CLIExitedDuringReplay(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	code := 0
	go func() {
		writeLine(t, server, ServerMsg{Type: "replay", Seq: 1, Line: "line1"})
		writeLine(t, server, ServerMsg{Type: "cli_exited", Code: &code})
	}()

	replays, err := handle.DrainReplay()
	if err != nil {
		t.Fatalf("DrainReplay: %v", err)
	}
	if len(replays) != 2 {
		t.Fatalf("got %d messages, want 2 (1 replay + cli_exited)", len(replays))
	}
	if replays[0].Type != "replay" {
		t.Errorf("replays[0].Type = %q, want replay", replays[0].Type)
	}
	if replays[1].Type != "cli_exited" {
		t.Errorf("replays[1].Type = %q, want cli_exited", replays[1].Type)
	}
}

func TestShimHandle_DrainReplay_Timeout(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()

	// Server never sends replay_done — close after short delay to trigger error
	go func() {
		time.Sleep(50 * time.Millisecond)
		server.Close()
	}()

	_, err := handle.DrainReplay()
	if err == nil {
		t.Error("expected error when server closes without replay_done, got nil")
	}
}

func TestShimHandle_DrainReplay_SkipsUnknownMessages(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	go func() {
		writeLine(t, server, ServerMsg{Type: "replay", Seq: 1, Line: "a"})
		server.Write([]byte(`{"type":"unknown_surprise"}` + "\n")) //nolint:errcheck
		writeLine(t, server, ServerMsg{Type: "replay_done", Count: 1})
	}()

	replays, err := handle.DrainReplay()
	if err != nil {
		t.Fatalf("DrainReplay: %v", err)
	}
	if len(replays) != 1 {
		t.Errorf("got %d replay messages, want 1", len(replays))
	}
}

// --- ShimHandle.Close / Detach / Shutdown ---

func TestShimHandle_Close_SignalsDone(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer server.Close()

	handle.Close()
	select {
	case <-handle.ClientDone:
		// expected
	case <-time.After(time.Second):
		t.Fatal("ClientDone not closed after Close()")
	}
}

func TestShimHandle_Close_Idempotent(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer server.Close()

	handle.Close()
	handle.Close() // must not panic
}

func TestShimHandle_Detach_SendsDetachMsg(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer server.Close()

	gotCh := make(chan ClientMsg, 1)
	go func() {
		var msg ClientMsg
		readLineMsgConn(server, &msg)
		gotCh <- msg
	}()

	go handle.Detach()

	select {
	case got := <-gotCh:
		if got.Type != "detach" {
			t.Errorf("msg.Type = %q, want detach", got.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for detach message")
	}
}

func TestShimHandle_Shutdown_SendsShutdownMsg(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer server.Close()

	gotCh := make(chan ClientMsg, 1)
	go func() {
		var msg ClientMsg
		readLineMsgConn(server, &msg)
		gotCh <- msg
	}()

	go handle.Shutdown()

	select {
	case got := <-gotCh:
		if got.Type != "shutdown" {
			t.Errorf("msg.Type = %q, want shutdown", got.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shutdown message")
	}
}

// --- ShimHandle.SendMsg concurrent safety (WriteMu) ---

func TestShimHandle_SendMsg_ConcurrentSafe(t *testing.T) {
	handle, server := newTestHandlePair(t)
	defer handle.Conn.Close()
	defer server.Close()

	const n = 50
	receivedCount := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(server)
		count := 0
		for sc.Scan() {
			count++
			if count == n {
				break
			}
		}
		receivedCount <- count
	}()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle.SendMsg(ClientMsg{Type: "ping"}) //nolint:errcheck
		}()
	}
	wg.Wait()

	select {
	case count := <-receivedCount:
		if count != n {
			t.Errorf("received %d messages, want %d", count, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for all messages to be received")
	}
}

// --- Manager defaults and configuration ---

func TestNewManager_Defaults(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})

	if m.maxShims != 50 {
		t.Errorf("maxShims = %d, want 50", m.maxShims)
	}
	if m.bufferSize != 10000 {
		t.Errorf("bufferSize = %d, want 10000", m.bufferSize)
	}
	if m.maxBufBytes != 50*1024*1024 {
		t.Errorf("maxBufBytes = %d, want 50MB", m.maxBufBytes)
	}
	if m.idleTimeout != 4*time.Hour {
		t.Errorf("idleTimeout = %v, want 4h", m.idleTimeout)
	}
	if m.watchdogTimeout != 30*time.Minute {
		t.Errorf("watchdogTimeout = %v, want 30m", m.watchdogTimeout)
	}
	if m.shims == nil {
		t.Error("shims map should be initialized")
	}
}

func TestNewManager_CustomConfig(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{
		StateDir:        dir,
		MaxShims:        10,
		BufferSize:      500,
		MaxBufBytes:     1024,
		IdleTimeout:     2 * time.Hour,
		WatchdogTimeout: 5 * time.Minute,
	})

	if m.maxShims != 10 {
		t.Errorf("maxShims = %d, want 10", m.maxShims)
	}
	if m.bufferSize != 500 {
		t.Errorf("bufferSize = %d, want 500", m.bufferSize)
	}
	if m.maxBufBytes != 1024 {
		t.Errorf("maxBufBytes = %d, want 1024", m.maxBufBytes)
	}
	if m.idleTimeout != 2*time.Hour {
		t.Errorf("idleTimeout = %v, want 2h", m.idleTimeout)
	}
	if m.watchdogTimeout != 5*time.Minute {
		t.Errorf("watchdogTimeout = %v, want 5m", m.watchdogTimeout)
	}
}

func TestManager_Remove(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})

	key := "test:key"
	m.mu.Lock()
	m.shims[key] = &ShimHandle{}
	m.mu.Unlock()

	m.Remove(key)

	m.mu.Lock()
	_, exists := m.shims[key]
	m.mu.Unlock()

	if exists {
		t.Error("Remove did not delete the key from shims map")
	}
}

func TestManager_Remove_NonExistentKey(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	m.Remove("nonexistent:key") // must not panic
}

func TestManager_CLIPath(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir, CLIPath: "/usr/bin/claude"})
	if m.CLIPath() != "/usr/bin/claude" {
		t.Errorf("CLIPath() = %q, want /usr/bin/claude", m.CLIPath())
	}
}

// --- filterShimEnv ---

func TestFilterShimEnv_AllowsExpectedPrefixes(t *testing.T) {
	tests := []struct {
		env     string
		allowed bool
	}{
		// Allowed
		{"HOME=/home/user", true},
		{"USER=alice", true},
		{"LOGNAME=alice", true},
		{"PATH=/usr/bin:/bin", true},
		{"SHELL=/bin/bash", true},
		{"TERM=xterm-256color", true},
		{"TMPDIR=/tmp", true},
		{"TMP=/tmp", true},
		{"TEMP=/tmp", true},
		{"LANG=en_US.UTF-8", true},
		{"LC_ALL=C", true},
		{"LC_MESSAGES=en_US.UTF-8", true},
		{"TZ=UTC", true},
		{"XDG_RUNTIME_DIR=/run/user/1000", true},
		{"XDG_CONFIG_HOME=/home/user/.config", true},
		{"ANTHROPIC_API_KEY=sk-abc", true},
		{"CLAUDE_MODEL=claude-opus", true},
		{"AWS_REGION=us-east-1", true},
		{"AWS_ACCESS_KEY_ID=AKIA...", true},
		{"SSH_AUTH_SOCK=/tmp/ssh.sock", true},
		{"GIT_AUTHOR_NAME=Alice", true},
		{"GOPATH=/home/user/go", true},
		{"GOROOT=/usr/local/go", true},
		{"GOBIN=/home/user/go/bin", true},
		{"CARGO_HOME=/home/user/.cargo", true},
		{"RUSTUP_HOME=/home/user/.rustup", true},
		{"NVM_DIR=/home/user/.nvm", true},
		{"NODE_ENV=production", true},
		{"NPM_TOKEN=abc", true},
		{"PYTHON=/usr/bin/python3", true},
		{"PYTHONPATH=/usr/lib/python3", true},
		{"VIRTUAL_ENV=/home/user/venv", true},
		{"CONDA_PREFIX=/opt/conda", true},
		{"JAVA_HOME=/usr/lib/jvm/java-17", true},

		// Blocked
		{"DATABASE_URL=postgres://...", false},
		{"REDIS_PASSWORD=secret", false},
		{"SECRET_KEY=abc123", false},
		{"SLACK_TOKEN=xoxb-...", false},
		{"GITHUB_TOKEN=ghp_...", false},
		{"OPENAI_API_KEY=sk-...", false},
		{"STRIPE_SECRET=sk_live_...", false},
		{"MYSQL_PASSWORD=root", false},
		{"POSTGRES_PASSWORD=password", false},
		{"FOO=bar", false},
		{"DISPLAY=:0", false},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			result := filterShimEnv([]string{tc.env})
			got := len(result) == 1
			if got != tc.allowed {
				t.Errorf("filterShimEnv(%q) included=%v, want %v", tc.env, got, tc.allowed)
			}
		})
	}
}

func TestFilterShimEnv_PreservesOrder(t *testing.T) {
	input := []string{
		"HOME=/home/user",
		"SECRET=blocked",
		"PATH=/bin",
		"REDIS_URL=blocked",
		"ANTHROPIC_API_KEY=allowed",
	}
	got := filterShimEnv(input)
	want := []string{"HOME=/home/user", "PATH=/bin", "ANTHROPIC_API_KEY=allowed"}

	if len(got) != len(want) {
		t.Fatalf("filterShimEnv len = %d, want %d; got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterShimEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFilterShimEnv_EmptyInput(t *testing.T) {
	if len(filterShimEnv(nil)) != 0 {
		t.Error("filterShimEnv(nil) should return empty")
	}
	if len(filterShimEnv([]string{})) != 0 {
		t.Error("filterShimEnv([]) should return empty")
	}
}

func TestFilterShimEnv_AllBlocked(t *testing.T) {
	input := []string{"SECRET=foo", "DATABASE=bar", "MYSQL_PWD=baz"}
	if len(filterShimEnv(input)) != 0 {
		t.Error("expected all blocked")
	}
}

// --- Manager.DetachAll / StopAll ---

func TestManager_DetachAll_Empty(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	m.DetachAll() // must not panic
}

func TestManager_StopAll_Empty(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})
	m.StopAll(t.Context()) // must not panic
}

func TestManager_DetachAll_SendsDetach(t *testing.T) {
	dir := t.TempDir()
	m := mustNewManager(t, ManagerConfig{StateDir: dir})

	const n = 3
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
		m.shims[fmt.Sprintf("key%d", i)] = handle
		m.mu.Unlock()
	}

	// For each server end, read one message concurrently
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
		m.DetachAll()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DetachAll did not complete in time")
	}

	// Drain received messages
	received := 0
	timeout := time.After(2 * time.Second)
	for received < n {
		select {
		case msg := <-msgCh:
			if msg.Type != "detach" {
				t.Errorf("got %q, want detach", msg.Type)
			}
			received++
		case <-timeout:
			t.Fatalf("timeout: only received %d/%d detach messages", received, n)
		}
	}
}
