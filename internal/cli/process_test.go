package cli

import (
	"bufio"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// nopWriteCloser is a no-op io.WriteCloser used as stdin stub in pipe-based tests.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// newTestProcess builds a Process backed by an io.Pipe instead of a real subprocess.
// Callers must start readLoop manually; close the returned writer to signal EOF.
func newTestProcess(proto Protocol) (*Process, *io.PipeWriter) {
	pr, pw := io.Pipe()
	p := &Process{
		scanner:  bufio.NewScanner(pr),
		protocol: proto,
		stdin:    nopWriteCloser{},
		eventCh:  make(chan Event, 64),
		done:     make(chan struct{}),
		killCh:   make(chan struct{}),
		State:    StateSpawning,
	}
	p.scanner.Buffer(make([]byte, 0, maxScannerBufBytes), maxScannerBufBytes)
	return p, pw
}

// spawnCatProcess starts a real "cat" subprocess for lifecycle tests.
// Registered for cleanup so orphaned processes are always reaped.
func spawnCatProcess(t *testing.T) *Process {
	t.Helper()
	p, err := newProcess(context.Background(), "cat", nil, "", 0, 0, &ClaudeProtocol{})
	if err != nil {
		t.Fatalf("spawnCatProcess: %v", err)
	}
	t.Cleanup(func() { p.Kill() })
	return p
}

// spawnSleepProcess starts "sleep 100"; the process ignores stdin close.
func spawnSleepProcess(t *testing.T) *Process {
	t.Helper()
	p, err := newProcess(context.Background(), "sleep", []string{"100"}, "", 0, 0, &ClaudeProtocol{})
	if err != nil {
		t.Fatalf("spawnSleepProcess: %v", err)
	}
	t.Cleanup(func() { p.Kill() })
	return p
}

// --- Alive() ---

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

// --- IsRunning() ---

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

// --- Kill() ---

func TestProcess_Kill_Idempotent(t *testing.T) {
	p := spawnCatProcess(t)
	// Repeated sequential calls must not panic (killOnce/waitOnce guard all sections).
	p.Kill()
	p.Kill()
	p.Kill()
}

func TestProcess_Kill_ConcurrentSafe(t *testing.T) {
	p := spawnCatProcess(t)
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

// --- Close() ---

func TestProcess_Close_Graceful(t *testing.T) {
	p := spawnCatProcess(t)
	// readLoop must run so that done is closed when cat exits.
	p.startReadLoop()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		p.Close()
	}()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return within 3s on graceful path")
	}
	if p.Alive() {
		t.Error("Alive() = true after Close(), want false")
	}
}

func TestProcess_Close_TimeoutFallback(t *testing.T) {
	old := processCloseTimeout
	processCloseTimeout = 50 * time.Millisecond
	defer func() { processCloseTimeout = old }()

	// sleep ignores stdin close, so Close() must time out and fall back to Kill().
	// readLoop is intentionally not started: done never closes naturally, guaranteeing
	// the timeout branch is taken.
	p := spawnSleepProcess(t)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		p.Close()
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() timeout fallback did not return within 2s")
	}
}

// --- readLoop ---

func TestProcess_ReadLoop_ForwardsEventsToChannel(t *testing.T) {
	p, pw := newTestProcess(&ClaudeProtocol{})
	go p.readLoop()

	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.01}`,
	}
	for _, l := range lines {
		if _, err := pw.Write([]byte(l + "\n")); err != nil {
			t.Fatalf("pipe write: %v", err)
		}
	}
	pw.Close()

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

func TestProcess_ReadLoop_SetsStateDeadOnEOF(t *testing.T) {
	p, pw := newTestProcess(&ClaudeProtocol{})
	go p.readLoop()

	pw.Close() // immediate EOF

	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("done channel not closed after pipe EOF")
	}

	p.mu.Lock()
	state := p.State
	p.mu.Unlock()

	if state != StateDead {
		t.Errorf("State = %v after EOF, want StateDead", state)
	}
	if p.Alive() {
		t.Error("Alive() = true after readLoop exits via EOF, want false")
	}
}

func TestProcess_ReadLoop_SkipsInvalidJSON(t *testing.T) {
	p, pw := newTestProcess(&ClaudeProtocol{})
	go p.readLoop()

	pw.Write([]byte("not-valid-json\n"))                                         //nolint
	pw.Write([]byte(`{"type":"result","result":"ok","session_id":"s1"}` + "\n")) //nolint
	pw.Close()

	var got []Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}

	if len(got) != 1 || got[0].Type != "result" {
		t.Errorf("got %d events (types: %v), want 1 result event", len(got), eventTypes(got))
	}
}

func TestProcess_ReadLoop_SkipsEmptyLines(t *testing.T) {
	p, pw := newTestProcess(&ClaudeProtocol{})
	go p.readLoop()

	pw.Write([]byte("\n"))                                     //nolint
	pw.Write([]byte(`{"type":"result","result":"ok"}` + "\n")) //nolint
	pw.Close()

	var got []Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}

	if len(got) != 1 || got[0].Type != "result" {
		t.Errorf("got %d events, want 1 result; types=%v", len(got), eventTypes(got))
	}
}

func TestProcess_ReadLoop_SkipsHookEvents(t *testing.T) {
	p, pw := newTestProcess(&ClaudeProtocol{})
	go p.readLoop()

	// ClaudeProtocol returns empty Type for hook_started/hook_response — skipped.
	pw.Write([]byte(`{"type":"system","subtype":"hook_started"}` + "\n"))  //nolint
	pw.Write([]byte(`{"type":"system","subtype":"hook_response"}` + "\n")) //nolint
	pw.Write([]byte(`{"type":"result","result":"ok"}` + "\n"))             //nolint
	pw.Close()

	var got []Event
	for ev := range p.eventCh {
		got = append(got, ev)
	}

	if len(got) != 1 || got[0].Type != "result" {
		t.Errorf("got %d events, want 1 result; types=%v", len(got), eventTypes(got))
	}
}

// TestProcess_ReadLoop_ExitsOnKillCh verifies that closing killCh unblocks a
// readLoop that is stuck waiting for eventCh space.
func TestProcess_ReadLoop_ExitsOnKillCh(t *testing.T) {
	pr, pw := io.Pipe()
	p := &Process{
		scanner:  bufio.NewScanner(pr),
		protocol: &ClaudeProtocol{},
		stdin:    nopWriteCloser{},
		eventCh:  make(chan Event), // zero-buffer: send always blocks without a receiver
		done:     make(chan struct{}),
		killCh:   make(chan struct{}),
		State:    StateSpawning,
	}
	p.scanner.Buffer(make([]byte, 0, maxScannerBufBytes), maxScannerBufBytes)
	go p.readLoop()

	// Give the goroutine an event to forward; it will block on the zero-buffer send.
	pw.Write([]byte(`{"type":"result","result":"x"}` + "\n")) //nolint

	// Closing killCh must unblock the select and cause readLoop to return.
	close(p.killCh)

	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Error("readLoop did not exit after killCh was closed")
	}

	pw.Close()
}

// --- State transitions ---

// TestProcess_StateTransitions walks the expected lifecycle:
// StateSpawning → StateRunning (Send enters) → StateReady (Send exits) → StateDead (EOF).
func TestProcess_StateTransitions(t *testing.T) {
	p, pw := newTestProcess(&ClaudeProtocol{})

	if p.State != StateSpawning {
		t.Errorf("initial state = %v, want StateSpawning", p.State)
	}

	go p.readLoop()

	// Simulate Send() acquiring the lock: Spawning → Running.
	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	if !p.IsRunning() {
		t.Error("IsRunning() = false after StateRunning, want true")
	}

	// Simulate Send() completing: Running → Ready.
	p.mu.Lock()
	if p.State == StateRunning {
		p.State = StateReady
	}
	p.mu.Unlock()

	if p.IsRunning() {
		t.Error("IsRunning() = true after StateReady, want false")
	}

	// EOF causes readLoop to set StateDead.
	pw.Close()

	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit after pipe EOF")
	}

	p.mu.Lock()
	final := p.State
	p.mu.Unlock()

	if final != StateDead {
		t.Errorf("final state = %v after EOF, want StateDead", final)
	}
}

// eventTypes is a test helper that extracts Type fields for diagnostic messages.
func eventTypes(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}
