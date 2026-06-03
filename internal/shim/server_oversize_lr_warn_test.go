package shim

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// warnCapture is a minimal slog.Handler that records Warn-level messages.
type warnCapture struct {
	count  atomic.Int64
	onWarn func(msg string)
}

func (h *warnCapture) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelWarn }
func (h *warnCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.count.Add(1)
		if h.onWarn != nil {
			h.onWarn(r.Message)
		}
	}
	return nil
}
func (h *warnCapture) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *warnCapture) WithGroup(_ string) slog.Handler      { return h }

// TestHandleClient_OversizeLimitedReader_LogsWarn pins [R090031-GO-3]:
// when a post-auth client sends data that exhausts the per-line LimitedReader
// (N reaches 0 without a newline), runCommandLoop must emit a slog.Warn
// before disconnecting. Without this guard the disconnect is silent, making
// oversize attacks invisible to operators.
//
// Mechanism: postAuthLR.N is reset to maxClientLineBytes()+1 at loop start.
// When the client sends exactly that many bytes without a newline the
// LimitedReader is drained to N==0, ReadBytes returns an error, and the new
// guard emits a Warn before returning.
func TestHandleClient_OversizeLimitedReader_LogsWarn(t *testing.T) {
	// Dial down the per-line limit so the test payload is small (128 B).
	const testLimit = 128
	origLine := setMaxClientLineBytes(testLimit)
	defer setMaxClientLineBytes(origLine)

	// Also dial down the session cap so it does not fire first.
	origSession := setMaxClientSessionBytes(int64(testLimit) * 10000)
	defer setMaxClientSessionBytes(origSession)

	// Capture Warn-level slog output before starting the server.
	warnCh := make(chan string, 8)
	handler := &warnCapture{
		onWarn: func(msg string) { warnCh <- msg },
	}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	_, client, cleanup := setupShimServerWithClient(t)
	defer cleanup()

	// Build a payload that is exactly testLimit+1 bytes with NO trailing
	// newline. The LimitedReader (reset to testLimit+1 per iteration) will
	// be fully consumed before bufio sees a line terminator; ReadBytes then
	// returns an error with postAuthLR.N == 0, triggering the new warn path.
	//
	// We send raw bytes rather than a valid JSON envelope to guarantee the
	// total byte count is exactly right.
	oversizePayload := strings.Repeat("X", testLimit+1)
	msg := ClientMsg{Type: "write", Line: oversizePayload}
	data, _ := json.Marshal(msg)
	// Intentionally omit the '\n' so bufio never gets a line terminator.
	client.conn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	client.writer.Write(data)                                 //nolint:errcheck
	client.writer.Flush()                                     //nolint:errcheck
	client.conn.SetWriteDeadline(time.Time{})                 //nolint:errcheck

	// The shim must disconnect and emit a Warn within 5 s.
	deadline := time.After(5 * time.Second)
	gotWarn := false
	for !gotWarn {
		select {
		case m := <-warnCh:
			if strings.Contains(m, "per-line byte limit") {
				gotWarn = true
			}
		case <-deadline:
			t.Fatalf("no oversize warn emitted within timeout (warn count=%d)",
				handler.count.Load())
		}
	}
}
