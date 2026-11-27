package shim

import (
	"bufio"
	"context"
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

	replays, err := handle.DrainReplay(context.Background())
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

	replays, err := handle.DrainReplay(context.Background())
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

	replays, err := handle.DrainReplay(context.Background())
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

	_, err := handle.DrainReplay(context.Background())
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

	replays, err := handle.DrainReplay(context.Background())
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

func TestShimHandle_DetachAndShutdownSendTheirMsg(t *testing.T) {
	for want, call := range map[string]func(*ShimHandle){
		"detach":   (*ShimHandle).Detach,
		"shutdown": (*ShimHandle).Shutdown,
	} {
		t.Run(want, func(t *testing.T) {
			handle, server := newTestHandlePair(t)
			defer server.Close()
			gotCh := make(chan ClientMsg, 1)
			go func() {
				var msg ClientMsg
				readLineMsgConn(server, &msg)
				gotCh <- msg
			}()
			go call(handle)
			select {
			case got := <-gotCh:
				if got.Type != want {
					t.Errorf("msg.Type = %q, want %s", got.Type, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timeout waiting for %s message", want)
			}
		})
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

// --- Manager.Remove ---

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

// --- Manager.DetachAll / StopAll ---

// DetachAll and StopAll send every registered shim its detach / shutdown.
func TestManager_DetachAllAndStopAll(t *testing.T) {
	for _, tc := range []struct {
		want string
		call func(*Manager, context.Context)
	}{
		{"detach", func(m *Manager, _ context.Context) { m.DetachAll() }},
		{"shutdown", (*Manager).StopAll},
	} {
		t.Run(tc.want, func(t *testing.T) {
			m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
			const n = 3
			msgCh := make(chan ClientMsg, n)
			for i := range n {
				c, srv := net.Pipe()
				m.mu.Lock()
				m.shims[fmt.Sprintf("key%d", i)] = &ShimHandle{
					Conn: c, Reader: bufio.NewReader(c), Writer: bufio.NewWriter(c), ClientDone: make(chan struct{}),
				}
				m.mu.Unlock()
				go func() {
					var msg ClientMsg
					err := readLineMsgConn(srv, &msg)
					srv.Close()
					if err == nil {
						msgCh <- msg
					}
				}()
			}

			done := make(chan struct{})
			go func() {
				tc.call(m, t.Context())
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not complete in time", tc.want)
			}
			timeout := time.After(2 * time.Second)
			for received := 0; received < n; received++ {
				select {
				case msg := <-msgCh:
					if msg.Type != tc.want {
						t.Errorf("got %q, want %s", msg.Type, tc.want)
					}
				case <-timeout:
					t.Fatalf("timeout: only received %d/%d %s messages", received, n, tc.want)
				}
			}
		})
	}
}
