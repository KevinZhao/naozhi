package persist

// Tests for R20260607-GO-003: dropInMemoryLocked logs (not swallows) the
// error returned by perKeyWriter.close().

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

// TestDropInMemoryLocked_LogsCloseError verifies that a close error on the
// per-key writer is emitted via slog.Warn rather than silently discarded.
// The test registers a capture handler, injects a pre-closed logFile to
// force an error, and asserts the warning is recorded.
func TestDropInMemoryLocked_LogsCloseError(t *testing.T) {
	// Create a temporary file and close it early so that w.close() returns
	// "file already closed" from logFile.Close().
	f, err := os.CreateTemp(t.TempDir(), "drop-test-*.log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	// Pre-close the file so that logFile.Close() fails.
	f.Close()

	w := &perKeyWriter{logFile: f}

	// Capture slog output: install a handler that records Warn records.
	var warned bool
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&captureWarnHandler{onWarn: func(msg string) {
		if msg == "event log persist: close on drop failed" {
			warned = true
		}
	}}))

	var p Persister
	p.writers = map[string]*perKeyWriter{"testkey": w}
	p.dropInMemoryLocked("testkey")

	if _, exists := p.writers["testkey"]; exists {
		t.Error("expected writer to be removed from map after drop")
	}
	if !warned {
		t.Error("expected slog.Warn for close error, but none was recorded")
	}
}

// captureWarnHandler is a minimal slog.Handler that calls onWarn for
// Warn-level records and forwards everything else to the default handler.
type captureWarnHandler struct {
	onWarn func(msg string)
}

func (h *captureWarnHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (h *captureWarnHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && h.onWarn != nil {
		h.onWarn(r.Message)
	}
	return nil
}
func (h *captureWarnHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureWarnHandler) WithGroup(_ string) slog.Handler      { return h }
