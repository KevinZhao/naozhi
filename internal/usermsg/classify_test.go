package usermsg

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/session"
)

// TestClassify_SentinelToCode pins the sentinel→Code mapping (#1413). This is
// the half of usermsg that legitimately depends on cli/session; the codeText
// table is tested separately in TestCodeText_NoUnknownRow without importing
// either package, demonstrating the decoupling.
func TestClassify_SentinelToCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		key  string
		want Code
	}{
		{"MaxProcs", session.ErrMaxProcs, "", CodeMaxProcs},
		{"MaxExempt", session.ErrMaxExemptSessions, "", CodeMaxExemptSessions},
		{"NoCLIWrapper", session.ErrNoCLIWrapper, "", CodeNoCLIWrapper},
		{"Asleep regular key", session.ErrNoActiveProcess, "feishu:p2p:u_x:agent", CodeSessionAsleep},
		{"Asleep cron key", session.ErrNoActiveProcess, "cron:slug", CodeCronAsleep},
		{"NoOutputTimeout", clierr.ErrNoOutputTimeout, "", CodeTimeout},
		{"TotalTimeout", clierr.ErrTotalTimeout, "", CodeTimeout},
		{"OrphanedSlot maps to timeout", clierr.ErrOrphanedSlot, "", CodeTimeout},
		{"ProcessExited", clierr.ErrProcessExited, "", CodeProcessExited},
		{"AbortedByUrgent", clierr.ErrAbortedByUrgent, "", CodeAbortedByUrgent},
		{"ReconnectedUnknown", clierr.ErrReconnectedUnknown, "", CodeReconnectedUnknown},
		{"SessionReset", clierr.ErrSessionReset, "", CodeSessionReset},
		{"TooManyPending", clierr.ErrTooManyPending, "", CodeTooManyPending},
		{"ProcessBusy", clierr.ErrProcessBusy, "", CodeProcessBusy},
		{"MessageTooLarge", clierr.ErrMessageTooLarge, "", CodeMessageTooLarge},
		{"Canceled", context.Canceled, "", CodeRestarting},
		{"DeadlineExceeded", context.DeadlineExceeded, "", CodeRestarting},
		{"RouterStopped", session.ErrRouterStopped, "", CodeRestarting},
		{"wrapped sentinel via errors.Is", wrapErr(session.ErrMaxProcs), "", CodeMaxProcs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.err, tt.key); got != tt.want {
				t.Errorf("classify(%v, %q) = %d, want %d", tt.err, tt.key, got, tt.want)
			}
		})
	}
}

// TestCodeText_NoUnknownRow pins two presentation-layer invariants that need
// NO cli/session import — proving the text table is decoupled:
//  1. CodeUnknown has no codeText row (it must fall through to the generic
//     hint, not silently render an empty string).
//  2. Every other named Code DOES have a row, so textForCode never returns
//     the generic hint for a real classification.
func TestCodeText_NoUnknownRow(t *testing.T) {
	t.Parallel()

	if _, ok := codeText[CodeUnknown]; ok {
		t.Error("CodeUnknown must not have a codeText row; it must use the generic fallback")
	}
	if got := textForCode(CodeUnknown); got != genericRetryHint {
		t.Errorf("textForCode(CodeUnknown) = %q, want generic hint %q", got, genericRetryHint)
	}

	// All real codes (everything after CodeUnknown up to CodeRestarting) must
	// map to a non-empty, non-generic label.
	for c := CodeUnknown + 1; c <= CodeRestarting; c++ {
		txt, ok := codeText[c]
		if !ok {
			t.Errorf("Code %d has no codeText row — every real classification must map to text", int(c))
			continue
		}
		if txt == "" || txt == genericRetryHint {
			t.Errorf("Code %d maps to %q, want a distinct non-empty label", int(c), txt)
		}
	}
}
