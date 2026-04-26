package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"
)

// shimTestPair creates a Process connected to a fake shim via net.Pipe.
// The returned shimEnd can be used to send shim protocol messages.
// Close the returned shimEnd to simulate shim disconnect.
func shimTestPair(proto Protocol) (*Process, *shimTestServer) {
	clientConn, serverConn := net.Pipe()

	p := newShimProcess(
		clientConn,
		bufio.NewReader(clientConn),
		bufio.NewWriter(clientConn),
		proto, 0, 0,
		0, 0,
	)

	srv := &shimTestServer{
		conn:   serverConn,
		writer: bufio.NewWriter(serverConn),
		seq:    0,
	}

	return p, srv
}

// shimTestServer simulates the shim side of the connection for tests.
type shimTestServer struct {
	conn   net.Conn
	writer *bufio.Writer
	seq    int64
	mu     sync.Mutex
}

// SendStdout sends a shim stdout message wrapping the given raw NDJSON line.
func (s *shimTestServer) SendStdout(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	msg := struct {
		Type string `json:"type"`
		Seq  int64  `json:"seq"`
		Line string `json:"line"`
	}{"stdout", s.seq, line}
	data, _ := json.Marshal(msg)
	s.writer.Write(data)         //nolint:errcheck
	s.writer.Write([]byte{'\n'}) //nolint:errcheck
	s.writer.Flush()             //nolint:errcheck
}

// SendCLIExited sends a shim cli_exited message.
func (s *shimTestServer) SendCLIExited(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := struct {
		Type string `json:"type"`
		Code int    `json:"code"`
	}{"cli_exited", code}
	data, _ := json.Marshal(msg)
	s.writer.Write(data)         //nolint:errcheck
	s.writer.Write([]byte{'\n'}) //nolint:errcheck
	s.writer.Flush()             //nolint:errcheck
}

// Close closes the server side of the connection.
func (s *shimTestServer) Close() {
	s.conn.Close()
}

// --- Tests ---

func TestProcess_Alive_TrueWhenDoneOpen(t *testing.T) {
	p := &Process{done: make(chan struct{})}
	if !p.Alive() {
		t.Error("Alive() = false, want true when done is open")
	}
}

func TestProcess_Alive_FalseAfterDoneClosed(t *testing.T) {
	p := &Process{done: make(chan struct{})}
	close(p.done)
	if p.Alive() {
		t.Error("Alive() = true, want false after done is closed")
	}
}

func TestProcess_IsRunning(t *testing.T) {
	cases := []struct {
		state ProcessState
		want  bool
	}{
		{StateSpawning, false},
		{StateReady, false},
		{StateRunning, true},
		{StateDead, false},
	}
	for _, tc := range cases {
		p := &Process{State: tc.state}
		if got := p.IsRunning(); got != tc.want {
			t.Errorf("state=%v: IsRunning() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestProcess_ReadLoop_ForwardsEventsToChannel(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	// Send two events wrapped in shim stdout messages
	srv.SendStdout(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.01}`)
	srv.SendCLIExited(0)

	var got []Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Type != "assistant" {
		t.Errorf("event[0].Type = %q, want assistant", got[0].Type)
	}
	if got[1].Type != "result" || got[1].Result != "done" || got[1].SessionID != "s1" {
		t.Errorf("event[1] = %+v, want result/done/s1", got[1])
	}
}

func TestProcess_ReadLoop_SetsStateDeadOnCLIExited(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	srv.SendCLIExited(0)

	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("done channel not closed after cli_exited")
	}

	p.mu.Lock()
	state := p.State
	p.mu.Unlock()

	if state != StateDead {
		t.Errorf("State = %v after cli_exited, want StateDead", state)
	}
}

func TestProcess_ReadLoop_SetsStateDeadOnDisconnect(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	srv.Close() // simulate shim disconnect

	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("done channel not closed after disconnect")
	}

	if p.Alive() {
		t.Error("Alive() = true after disconnect, want false")
	}
}

func TestProcess_ReadLoop_SkipsInvalidJSON(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	// Send an invalid stdout line, then a valid one
	srv.SendStdout("not-valid-json")
	srv.SendStdout(`{"type":"result","result":"ok","session_id":"s1"}`)
	srv.SendCLIExited(0)

	var got []Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}

	if len(got) != 1 || got[0].Type != "result" {
		t.Errorf("got %d events (types: %v), want 1 result event", len(got), eventTypes(got))
	}
}

func TestProcess_ReadLoop_SkipsHookEvents(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	srv.SendStdout(`{"type":"system","subtype":"hook_started"}`)
	srv.SendStdout(`{"type":"system","subtype":"hook_response"}`)
	srv.SendStdout(`{"type":"result","result":"ok"}`)
	srv.SendCLIExited(0)

	var got []Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}

	if len(got) != 1 || got[0].Type != "result" {
		t.Errorf("got %d events, want 1 result; types=%v", len(got), eventTypes(got))
	}
}

func TestProcess_ReadLoop_ExitsOnKillCh(t *testing.T) {
	p, _ := shimTestPair(&ClaudeProtocol{})

	// Use zero-buffer eventCh to force block
	p.eventCh = make(chan Event)
	go p.readLoop()

	// The readLoop is blocked waiting on shimR.ReadBytes. Close killCh won't
	// unblock it directly, but killing should work via the select in readLoop.
	// For this test, we verify Kill() closes killCh.
	p.Kill()

	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Error("readLoop did not exit after Kill()")
	}
}

func TestProcess_StateTransitions(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})

	if p.State != StateSpawning {
		t.Errorf("initial state = %v, want StateSpawning", p.State)
	}

	go p.readLoop()

	// Wait for readLoop to set StateReady
	time.Sleep(10 * time.Millisecond)

	// Simulate Send() acquiring lock: Ready → Running
	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	if !p.IsRunning() {
		t.Error("IsRunning() = false after StateRunning, want true")
	}

	// Simulate Send() completing: Running → Ready
	p.mu.Lock()
	if p.State == StateRunning {
		p.State = StateReady
	}
	p.mu.Unlock()

	if p.IsRunning() {
		t.Error("IsRunning() = true after StateReady, want false")
	}

	// CLI exit causes StateDead
	srv.SendCLIExited(0)

	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit after cli_exited")
	}

	p.mu.Lock()
	final := p.State
	p.mu.Unlock()

	if final != StateDead {
		t.Errorf("final state = %v, want StateDead", final)
	}
}

func TestProcess_Kill_Idempotent(t *testing.T) {
	p, _ := shimTestPair(&ClaudeProtocol{})
	// Repeated calls must not panic
	p.Kill()
	p.Kill()
	p.Kill()
}

func TestProcess_Kill_ConcurrentSafe(t *testing.T) {
	p, _ := shimTestPair(&ClaudeProtocol{})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Kill()
		}()
	}
	wg.Wait()
}

// TestProcess_Kill_SendsSIGUSR2ToShim locks in the behaviour added for
// UCCLEP-2026-04-26: when the Process carries a non-zero shimPID, Kill
// must send SIGUSR2 so the shim's immediate-shutdown path runs and the
// socket file is unlinked. Without this, the Kill fallback from Close's
// timeout still leaves the socket listening for up to 30s and the next
// StartShim for the same key fails the dial-first guard.
//
// Uses the test process's own PID as the shim PID so the handler observes
// the signal without spawning a child; the handler is removed at cleanup
// to avoid leaking SIGUSR2 handling into other tests in the same binary.
func TestProcess_Kill_SendsSIGUSR2ToShim(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	t.Cleanup(func() { signal.Stop(sigCh) })

	p, _ := shimTestPair(&ClaudeProtocol{})
	p.shimPID = os.Getpid()

	p.Kill()

	select {
	case <-sigCh:
		// good: Kill sent SIGUSR2 to shim
	case <-time.After(2 * time.Second):
		t.Fatal("Kill() did not send SIGUSR2 within 2s")
	}
}

// TestProcess_Kill_NoSIGUSR2WhenShimPIDZero ensures the fallback signal
// path is strictly gated on shimPID > 0 — tests and legacy callers that
// construct Process without hello data must not trigger a misdirected
// signal to PID 0 (which is "send to every process in the caller's
// process group" on Unix, a dangerous broadcast).
func TestProcess_Kill_NoSIGUSR2WhenShimPIDZero(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	t.Cleanup(func() { signal.Stop(sigCh) })

	p, _ := shimTestPair(&ClaudeProtocol{})
	if p.shimPID != 0 {
		t.Fatalf("precondition: shimPID = %d, want 0", p.shimPID)
	}

	p.Kill()

	select {
	case <-sigCh:
		t.Fatal("Kill() sent SIGUSR2 despite shimPID == 0")
	case <-time.After(200 * time.Millisecond):
		// good: no signal
	}
}

// Retained from original tests
func TestParseAgentInput(t *testing.T) {
	t.Run("label priority", func(t *testing.T) {
		cases := []struct {
			name  string
			input string
			want  string
		}{
			{"subagent_type wins", `{"subagent_type":"Explore","name":"my-agent","team_name":"team1"}`, "Explore"},
			{"name fallback", `{"name":"my-agent","team_name":"team1"}`, "my-agent"},
			{"team_name fallback", `{"team_name":"team1","description":"do stuff"}`, "team1"},
			{"all empty", `{"description":"do stuff"}`, ""},
			{"empty input", ``, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := parseAgentInput(json.RawMessage(tc.input)).label()
				if got != tc.want {
					t.Errorf("label() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("run_in_background", func(t *testing.T) {
		cases := []struct {
			name  string
			input string
			want  bool
		}{
			{"true", `{"run_in_background":true,"team_name":"t1"}`, true},
			{"false explicit", `{"run_in_background":false}`, false},
			{"absent", `{"team_name":"t1"}`, false},
			{"empty input", ``, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := parseAgentInput(json.RawMessage(tc.input)).RunInBackground
				if got != tc.want {
					t.Errorf("RunInBackground = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("description", func(t *testing.T) {
		inp := parseAgentInput(json.RawMessage(`{"description":"do the thing","team_name":"t1"}`))
		if inp.Description != "do the thing" {
			t.Errorf("Description = %q, want %q", inp.Description, "do the thing")
		}
	})
}

func eventTypes(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// Unused import guard — context is used by test helpers that may be added later.
var _ = context.Background
