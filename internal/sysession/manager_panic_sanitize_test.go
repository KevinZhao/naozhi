package sysession

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// captureHandler records the value of the "recover" attribute from any
// log record so the panic-recover path can be inspected.
type captureHandler struct {
	recover string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "recover" {
			h.recover = a.Value.String()
			return false
		}
		return true
	})
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// TestRunOnce_PanicRecoverSanitised pins #2229: when a daemon Tick panics,
// the recover value is logged through SanitizeForLog so an attacker- or
// conversation-derived panic payload (control runes, oversized tail)
// cannot land raw in the logs. Pre-fix runOnce did slog.Error(..., "recover", r)
// with the unsanitised value.
func TestRunOnce_PanicRecoverSanitised(t *testing.T) {
	prev := slog.Default()
	cap := &captureHandler{}
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rawTail := strings.Repeat("a", 4096)
	d := &signalDaemon{
		name: "auto-titler",
		tickFn: func(context.Context, int32) (TickReport, error) {
			panic("leaked\x00\x07secret\x1b[31m: " + rawTail)
		},
	}
	rec := &daemonRecord{
		daemon:           d,
		tick:             time.Second,
		processStartedAt: time.Now(),
		runs:             newRunRing(),
	}
	m := &Manager{cfg: Config{TickTimeout: time.Second}}

	m.runOnce(context.Background(), rec, DaemonTriggerScheduled)

	got := cap.recover
	if got == "" {
		t.Fatalf("expected recover attr to be logged on panic")
	}
	for _, c := range []byte{0x00, 0x07, 0x1b} {
		if strings.IndexByte(got, c) >= 0 {
			t.Errorf("recover attr retains control byte 0x%02x: %q", c, got)
		}
	}
	if len(got) > 256 {
		t.Errorf("recover attr len=%d exceeds 256-byte cap", len(got))
	}
	if !strings.HasPrefix(got, "leaked") {
		t.Errorf("recover attr lost leading printable prefix: %q", got)
	}
}
