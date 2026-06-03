package cron

import (
	"os"
	"strings"
	"testing"
)

// TestRunStore_PreflightAndRetryLogMsgsDistinct pins R20260602-CR-2:
// the preflight over-cap warning (line ~699) and the retry-also-exceeded
// warning (line ~716) must have distinct message strings so log-based
// alerting can distinguish them.
//
// Before the fix both paths emitted
// "cron run: payload exceeds size cap; truncating result/prompt and retrying"
// making it impossible to tell whether the truncated path was taken early
// (pre-flight, no first marshal) or late (after marshal succeeded but the
// marshalled payload was still too large).
func TestRunStore_PreflightAndRetryLogMsgsDistinct(t *testing.T) {
	src, err := os.ReadFile("runstore.go")
	if err != nil {
		t.Fatalf("read runstore.go: %v", err)
	}
	body := string(src)

	// The preflight path must contain the new distinct message.
	const preflightMsg = "cron run: preflight over-cap: truncating result/prompt directly (skipping full marshal)"
	if !strings.Contains(body, preflightMsg) {
		t.Errorf("R20260602-CR-2: preflight over-cap slog.Warn message %q not found in runstore.go; "+
			"the two Warn paths must have distinct messages for log-based alerting", preflightMsg)
	}

	// The retry path must contain its own distinct message (unchanged from before).
	const retryMsg = "cron run: retry marshal also exceeded cap; run record dropped"
	if !strings.Contains(body, retryMsg) {
		t.Errorf("R20260602-CR-2: retry-exceeded slog.Warn message %q not found in runstore.go", retryMsg)
	}

	// The two messages must be different strings (enforce the invariant, not just
	// the current text, so a future copy-paste regression is caught immediately).
	if preflightMsg == retryMsg {
		t.Error("R20260602-CR-2: preflight and retry log messages are identical; they must differ")
	}
}
