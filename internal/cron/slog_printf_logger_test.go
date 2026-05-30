package cron

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestSlogPrintfLoggerEnabledGate verifies the R249-PERF-10 (#931) gate:
// when the default slog handler discards both Warn and Error, Printf emits
// nothing (and skips the fmt.Sprintf / strings work). When the handler
// accepts Warn, ordinary lines route to Warn and panic lines to Error.
func TestSlogPrintfLoggerEnabledGate(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	// Handler raised above Error: nothing should be emitted.
	var buf bytes.Buffer
	silent := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError + 1})
	slog.SetDefault(slog.New(silent))
	slogPrintfLogger{}.Printf("schedule parse error: %s", "bad spec")
	if buf.Len() != 0 {
		t.Fatalf("expected no output when Warn+Error disabled, got %q", buf.String())
	}

	// Handler at Warn: ordinary line -> WARN, panic line -> ERROR.
	buf.Reset()
	open := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(open))
	slogPrintfLogger{}.Printf("schedule parse error: %s", "bad spec")
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected WARN line, got %q", out)
	}

	buf.Reset()
	slogPrintfLogger{}.Printf("cron job %d: %s", 1, "panic recovered")
	out = buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("expected ERROR line for panic marker, got %q", out)
	}
}

// sanity: the gate uses Default().Enabled, ensure it agrees with the handler.
func TestSlogPrintfLoggerGateMatchesHandler(t *testing.T) {
	h := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError + 1})
	if h.Enabled(context.Background(), slog.LevelWarn) || h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("handler raised above Error should report Warn/Error disabled")
	}
}
