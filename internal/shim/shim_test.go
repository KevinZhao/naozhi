package shim

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestRingBuffer_PushAndLinesSince(t *testing.T) {
	b := NewRingBuffer(5, 1024)

	for i := 0; i < 3; i++ {
		b.Push([]byte("line"))
	}

	if b.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", b.Count())
	}

	lines := b.LinesSince(0)
	if len(lines) != 3 {
		t.Fatalf("LinesSince(0) returned %d lines, want 3", len(lines))
	}
	if lines[0].seq != 1 || lines[2].seq != 3 {
		t.Errorf("seq range = [%d, %d], want [1, 3]", lines[0].seq, lines[2].seq)
	}

	lines = b.LinesSince(2)
	if len(lines) != 1 || lines[0].seq != 3 {
		t.Errorf("LinesSince(2) = %d lines (seq=%d), want 1 line seq=3", len(lines), lines[0].seq)
	}
}

func TestRingBuffer_Eviction(t *testing.T) {
	b := NewRingBuffer(3, 1024)

	for i := 0; i < 5; i++ {
		b.Push([]byte("x"))
	}

	if b.Count() != 3 {
		t.Fatalf("Count() = %d, want 3 (capped at maxLines)", b.Count())
	}

	oldest, newest := b.SeqRange()
	if oldest != 3 || newest != 5 {
		t.Errorf("SeqRange = (%d, %d), want (3, 5)", oldest, newest)
	}
}

func TestRingBuffer_ByteLimit(t *testing.T) {
	b := NewRingBuffer(100, 20) // 20 bytes max

	b.Push([]byte("12345678")) // 8 bytes
	b.Push([]byte("12345678")) // 8 bytes, total 16
	b.Push([]byte("12345678")) // 8 bytes, would be 24 → evict oldest

	if b.Count() != 2 {
		t.Fatalf("Count() = %d, want 2 (byte limit enforced)", b.Count())
	}
	if b.Bytes() != 16 {
		t.Errorf("Bytes() = %d, want 16", b.Bytes())
	}
}

func TestRingBuffer_Empty(t *testing.T) {
	b := NewRingBuffer(10, 1024)

	if b.Count() != 0 {
		t.Errorf("Count() = %d, want 0", b.Count())
	}

	oldest, newest := b.SeqRange()
	if oldest != 0 || newest != 0 {
		t.Errorf("SeqRange = (%d, %d), want (0, 0)", oldest, newest)
	}

	lines := b.LinesSince(0)
	if len(lines) != 0 {
		t.Errorf("LinesSince(0) = %d lines, want 0", len(lines))
	}
}

func TestWatchdog_FiresOnTimeout(t *testing.T) {
	fired := make(chan struct{})
	w := NewWatchdog(50*time.Millisecond, func() {
		close(fired)
	})
	w.Start()

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not fire within 1s")
	}
}

func TestWatchdog_ResetPrevents(t *testing.T) {
	fired := make(chan struct{})
	w := NewWatchdog(100*time.Millisecond, func() {
		close(fired)
	})
	w.Start()

	// Reset before timeout
	time.Sleep(50 * time.Millisecond)
	w.Reset()
	time.Sleep(50 * time.Millisecond)
	w.Reset()

	// Should not have fired yet
	select {
	case <-fired:
		t.Fatal("watchdog fired despite resets")
	case <-time.After(50 * time.Millisecond):
		// good
	}

	w.Stop()
}

func TestWatchdog_StopPrevents(t *testing.T) {
	fired := make(chan struct{})
	w := NewWatchdog(50*time.Millisecond, func() {
		close(fired)
	})
	w.Start()
	w.Stop()

	select {
	case <-fired:
		t.Fatal("watchdog fired after Stop()")
	case <-time.After(150 * time.Millisecond):
		// good
	}
}

// TestWatchdog_ResetStaleCallbackNoOp verifies the generation-counter fix:
// a callback that was already scheduled at the time of Reset must not fire.
// We use a very short timeout and call Reset just before expiry, then confirm
// the watchdog eventually fires exactly once (the fresh generation), not twice.
func TestWatchdog_ResetStaleCallbackNoOp(t *testing.T) {
	var fireCount atomic.Int32
	w := NewWatchdog(30*time.Millisecond, func() {
		fireCount.Add(1)
	})
	w.Start()

	// Reset just before the first timer would fire; this invalidates the first
	// callback (generation 0 → 1) and schedules a new one.
	time.Sleep(20 * time.Millisecond)
	w.Reset()

	// Wait long enough for both the stale and the fresh callback to have run.
	time.Sleep(100 * time.Millisecond)

	if got := fireCount.Load(); got != 1 {
		t.Errorf("expected watchdog to fire exactly once, got %d", got)
	}
}

// TestWatchdog_StopInvalidatesCallback verifies that Stop increments the
// generation so a racing callback that fires after Stop is a no-op.
func TestWatchdog_StopInvalidatesCallback(t *testing.T) {
	var fireCount atomic.Int32
	w := NewWatchdog(20*time.Millisecond, func() {
		fireCount.Add(1)
	})
	w.Start()

	// Stop just before the timer fires.
	time.Sleep(10 * time.Millisecond)
	w.Stop()

	// Allow time for any in-flight AfterFunc callback to execute.
	time.Sleep(50 * time.Millisecond)

	if got := fireCount.Load(); got != 0 {
		t.Errorf("expected watchdog not to fire after Stop, got %d fires", got)
	}
}

func TestProtocol_MarshalRoundtrip(t *testing.T) {
	msg := ServerMsg{
		Type: "stdout",
		Seq:  42,
		Line: `{"type":"result","result":"hello"}`,
	}
	data, err := msg.MarshalLine()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseServerMsg(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "stdout" || parsed.Seq != 42 || parsed.Line != msg.Line {
		t.Errorf("roundtrip mismatch: %+v", parsed)
	}
}

func TestProtocol_ClientMsg(t *testing.T) {
	raw := []byte(`{"type":"write","line":"{\"type\":\"user\"}"}`)
	msg, err := ParseClientMsg(raw)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != "write" || msg.Line != `{"type":"user"}` {
		t.Errorf("parse mismatch: %+v", msg)
	}
}

func TestState_WriteRead(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.json"

	original := State{
		ShimPID:   12345,
		CLIPID:    23456,
		Socket:    "/tmp/test.sock",
		AuthToken: "dGVzdA==",
		Key:       "feishu:d:alice:general",
		SessionID: "sess_abc",
		CLIAlive:  true,
	}

	if err := WriteStateFile(path, original); err != nil {
		t.Fatal(err)
	}

	loaded, err := ReadStateFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Version != stateVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, stateVersion)
	}
	if loaded.ShimPID != 12345 || loaded.Key != "feishu:d:alice:general" {
		t.Errorf("state mismatch: %+v", loaded)
	}
}

func TestKeyHash_Deterministic(t *testing.T) {
	h1 := KeyHash("feishu:d:alice:general")
	h2 := KeyHash("feishu:d:alice:general")
	if h1 != h2 {
		t.Errorf("KeyHash not deterministic: %s != %s", h1, h2)
	}

	h3 := KeyHash("feishu:d:bob:general")
	if h1 == h3 {
		t.Errorf("KeyHash collision: alice == bob")
	}
}
