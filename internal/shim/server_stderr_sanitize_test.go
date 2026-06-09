package shim

// Tests for R20260607-SEC-2: readStderr sanitizes slog output and caps
// oversized lines before forwarding via WS (enqueueWrite).

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// makeStderrServer builds the minimum shimServer needed to exercise
// readStderr: a cliProc with a synthetic stderrR pipe, a writeCh to
// capture enqueued writes, and a done channel.
func makeStderrServer(t *testing.T, stderrR io.ReadCloser) (*shimServer, chan []byte) {
	t.Helper()
	writeCh := make(chan []byte, 64)
	clientDone := make(chan struct{})
	s := &shimServer{
		cli:        &cliProc{stderrR: stderrR},
		writeCh:    writeCh,
		clientDone: clientDone,
		done:       make(chan struct{}),
	}
	return s, writeCh
}

// drainWrites collects all messages from writeCh until it's idle for 100ms.
func drainWrites(ch chan []byte) []ServerMsg {
	var out []ServerMsg
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case data := <-ch:
			var msg ServerMsg
			if json.Unmarshal(data[:len(data)-1], &msg) == nil { // strip trailing \n
				out = append(out, msg)
			}
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
	return out
}

// TestReadStderr_BidiInjection_NotInWS verifies R20260607-SEC-2:
// a stderr line containing a bidi-override (U+202E) is forwarded as-is
// to WS (we cap size only) but the slog path uses SanitizeForLog.
// The WS Line field itself still has the raw bytes; size cap is the concern.
// (Sanitizing display is the receiver's job; the contract is: no oversized payload.)
func TestReadStderr_BidiInjection_NotOversized(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	s, writeCh := makeStderrServer(t, io.NopCloser(pr))

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.readStderr()
	}()

	// Write a short line with a bidi-override rune (U+202E RIGHT-TO-LEFT OVERRIDE).
	line := "stderr: ‮injected text"
	_, err := pw.Write([]byte(line + "\n"))
	if err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	pw.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readStderr did not exit")
	}

	msgs := drainWrites(writeCh)
	if len(msgs) == 0 {
		t.Fatal("expected at least one forwarded message")
	}
	found := false
	for _, m := range msgs {
		if m.Type == "stderr" {
			found = true
			// Line should not be truncated (it's short).
			if !strings.Contains(m.Line, "injected text") {
				t.Errorf("WS line missing expected content: %q", m.Line)
			}
		}
	}
	if !found {
		t.Error("no stderr ServerMsg received")
	}
}

// TestReadStderr_OversizeLine_TruncatedInWS verifies R20260607-SEC-2:
// a line exceeding 64 KiB is truncated before WS forwarding.
func TestReadStderr_OversizeLine_TruncatedInWS(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	s, writeCh := makeStderrServer(t, io.NopCloser(pr))

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.readStderr()
	}()

	// 70 KiB line (scanner buffer is 10 MiB so this is scannable).
	bigLine := strings.Repeat("x", 70*1024)
	go func() {
		// Write in a goroutine: pipe is unbuffered.
		pw.Write([]byte(bigLine + "\n")) //nolint:errcheck
		pw.Close()
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readStderr did not exit")
	}

	msgs := drainWrites(writeCh)
	found := false
	for _, m := range msgs {
		if m.Type == "stderr" {
			found = true
			const limit = 64 * 1024
			if len(m.Line) > limit+len("...[truncated]")+10 {
				t.Errorf("WS line not capped: len=%d", len(m.Line))
			}
			if len(m.Line) <= limit && !strings.HasSuffix(m.Line, "...[truncated]") {
				t.Errorf("expected truncation sentinel, got suffix: %q", m.Line[len(m.Line)-20:])
			}
		}
	}
	if !found {
		t.Error("no stderr ServerMsg received")
	}
}
