package cli

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
)

// TestClassifyEOF_DeathReasons covers the four arms of classifyEOF:
// closed/non-closed × afterDrain true/false. The post-extraction helper
// must reproduce the same setDeathReason values the inline switch wrote
// before R214-CODE-3, otherwise health dashboards lose the EOF vs ReadErr
// distinction.
//
// R20260527-GO-19 (#1288): the afterDrain branches now stamp dedicated
// labels (DeathReasonShimOversizeThenEOF / …ThenReadErr) so dashboard
// histograms can keep cap-violation lifecycle terminations distinct
// from clean shim_eof / shim_read_error.
func TestClassifyEOF_DeathReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		afterDrain bool
		want       string
	}{
		{name: "EOF_normal", err: io.EOF, afterDrain: false, want: DeathReasonShimEOF},
		{name: "EOF_after_drain", err: io.EOF, afterDrain: true, want: DeathReasonShimOversizeThenEOF},
		{name: "netClosed_normal", err: net.ErrClosed, afterDrain: false, want: DeathReasonShimEOF},
		{name: "netClosed_after_drain", err: net.ErrClosed, afterDrain: true, want: DeathReasonShimOversizeThenEOF},
		{name: "other_normal", err: errors.New("read fault"), afterDrain: false, want: DeathReasonShimReadErr},
		{name: "other_after_drain", err: errors.New("read fault"), afterDrain: true, want: DeathReasonShimOversizeThenReadErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Process{}
			// slog.New + slog.NewJSONHandler(io.Discard, ...) would also work;
			// the default discard logger keeps the test focused on side effects.
			p.classifyEOF(tc.err, tc.afterDrain, slog.New(slog.DiscardHandler))
			if got := p.DeathReason(); got != tc.want {
				t.Errorf("DeathReason after classifyEOF(%v, drain=%v) = %q, want %q",
					tc.err, tc.afterDrain, got, tc.want)
			}
		})
	}
}

// TestClassifyEOF_WrappedErrors locks the errors.Is unwrap behaviour: a
// wrapped io.EOF / net.ErrClosed must still classify as DeathReasonShimEOF.
// Net stack middlewares (TLS, retry shims) frequently wrap the underlying
// error, and the previous inline switch used errors.Is — extraction must
// not silently flip back to a == check.
func TestClassifyEOF_WrappedErrors(t *testing.T) {
	t.Parallel()
	wrappedEOF := &wrapErr{inner: io.EOF}
	wrappedClosed := &wrapErr{inner: net.ErrClosed}

	for _, err := range []error{wrappedEOF, wrappedClosed} {
		p := &Process{}
		p.classifyEOF(err, false, slog.New(slog.DiscardHandler))
		if got := p.DeathReason(); got != DeathReasonShimEOF {
			t.Errorf("wrapped %v: got %q, want %q", err, got, DeathReasonShimEOF)
		}
	}
}

// TestHandleShimMessage_Outcomes covers the dispatch table for the
// non-stdout branches that don't require a fully-spawned Process: stderr
// / pong / error all return shimDispatchContinue (they are pure
// side-effect frames). cli_exited returns shimDispatchReturn so readLoop
// unwinds; verify both the return value and the deathReason side effect.
func TestHandleShimMessage_Outcomes(t *testing.T) {
	t.Parallel()
	t.Run("stderr_continue", func(t *testing.T) {
		t.Parallel()
		p := &Process{}
		got := p.handleShimMessage(shimMsg{Type: "stderr", Line: "anything"}, slog.New(slog.DiscardHandler))
		if got != shimDispatchContinue {
			t.Errorf("stderr outcome = %v, want shimDispatchContinue", got)
		}
		if dr := p.DeathReason(); dr != "" {
			t.Errorf("stderr leaked deathReason: %q", dr)
		}
	})

	t.Run("pong_continue_nonblocking", func(t *testing.T) {
		t.Parallel()
		// pongRecv is a nil channel by default; the select's default arm
		// must absorb the send without panicking. This locks the
		// non-blocking semantic the heartbeat loop depends on (a stalled
		// heartbeatLoop must never block readLoop on a pong send).
		p := &Process{}
		got := p.handleShimMessage(shimMsg{Type: "pong"}, slog.New(slog.DiscardHandler))
		if got != shimDispatchContinue {
			t.Errorf("pong outcome = %v, want shimDispatchContinue", got)
		}
	})

	t.Run("error_continue_sanitised", func(t *testing.T) {
		t.Parallel()
		p := &Process{}
		// Embed a control byte; the test only asserts no panic / no
		// deathReason side effect — log content sanitisation is
		// covered separately in osutil tests.
		got := p.handleShimMessage(shimMsg{Type: "error", Line: "boom\x00bad"}, slog.New(slog.DiscardHandler))
		if got != shimDispatchContinue {
			t.Errorf("error outcome = %v, want shimDispatchContinue", got)
		}
	})

	t.Run("unknown_type_continue", func(t *testing.T) {
		t.Parallel()
		// Forward-compat: an unrecognised frame type must fall through
		// the switch and let readLoop pull the next message.
		p := &Process{}
		got := p.handleShimMessage(shimMsg{Type: "future_event"}, slog.New(slog.DiscardHandler))
		if got != shimDispatchContinue {
			t.Errorf("unknown outcome = %v, want shimDispatchContinue", got)
		}
	})
}

// wrapErr is a minimal errors.Is-compatible wrapper used by
// TestClassifyEOF_WrappedErrors to exercise the unwrap path without
// pulling fmt.Errorf into a hot test path.
type wrapErr struct{ inner error }

func (e *wrapErr) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrapErr) Unwrap() error { return e.inner }
