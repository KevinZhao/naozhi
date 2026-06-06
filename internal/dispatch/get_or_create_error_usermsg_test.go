package dispatch

import (
	"context"
	"log/slog"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/usermsg"
)

// TestHandleGetOrCreateError_DelegatesToUsermsg pins R20260531-ARCH-1: the
// session-sentinel → user-message mapping is owned by usermsg.ForSendError.
// handleGetOrCreateError must not re-implement it; for every known sentinel
// (and the unknown fallback) it must return exactly what ForSendError(err, "")
// yields, so the two delivery paths can never drift.
func TestHandleGetOrCreateError_DelegatesToUsermsg(t *testing.T) {
	d := &Dispatcher{}
	lg := slog.Default()

	cases := []struct {
		name string
		err  error
	}{
		{"max_procs", session.ErrMaxProcs},
		{"max_exempt", session.ErrMaxExemptSessions},
		{"no_cli_wrapper", session.ErrNoCLIWrapper},
		{"context_canceled", context.Canceled},
		{"unknown", errUnknownForTest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cleanup, errMsg := d.handleGetOrCreateError(context.Background(), tc.err, lg)
			if cleanup != nil {
				cleanup()
			}
			want := usermsg.ForSendError(tc.err, "")
			if errMsg != want {
				t.Fatalf("errMsg = %q, want %q (must match usermsg.ForSendError)", errMsg, want)
			}
			if errMsg == "" {
				t.Fatalf("errMsg must be non-empty for %v", tc.err)
			}
		})
	}
}

var errUnknownForTest = &simpleErr{"some unexpected backend failure"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
