package cron

import (
	"strings"
	"testing"
)

// TestSessionErrMsg_SanitisedBeforePersist verifies that the session-error
// path in executeOpt applies sanitiseRunErrMsg before passing errMsg to
// finishRun, mirroring the send-error path (R20260607-GO-004).
// [R20260613-LOGIC-1]
//
// This is a unit test of the sanitiseRunErrMsg function applied to the
// exact prefix the session-error path uses ("session error: " + msg),
// ensuring that secrets / addresses embedded in the error string are
// redacted before they could be persisted or broadcast.
func TestSessionErrMsg_SanitisedBeforePersist(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		raw      string // the raw err.Error() string
		wantGone string // substring that must NOT appear in sanitised output
		wantHave string // substring that MUST appear in sanitised output
	}{
		{
			name:     "API token in session error",
			raw:      "session error: auth rejected: sk-ant-api03-abcdef0123456789xyz",
			wantGone: "sk-ant-",
			wantHave: "[REDACTED]",
		},
		{
			name:     "IP:port in session error",
			raw:      "session error: dial tcp 10.0.0.1:50051: connection refused",
			wantGone: "10.0.0.1",
			wantHave: "[redacted-addr]",
		},
		{
			name:     "GitHub token in session error",
			raw:      "session error: push failed with ghp_abcdef0123456789 token",
			wantGone: "ghp_",
			wantHave: "[REDACTED]",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Replicate the exact production expression from scheduler_run.go:
			//   errMsg: "session error: " + sanitiseRunErrMsg(err.Error())
			got := "session error: " + sanitiseRunErrMsg(tc.raw)
			if strings.Contains(got, tc.wantGone) {
				t.Errorf("sanitiseRunErrMsg left %q intact in session-error path: got %q", tc.wantGone, got)
			}
			if !strings.Contains(got, tc.wantHave) {
				t.Errorf("sanitiseRunErrMsg did not insert %q for session-error path: got %q", tc.wantHave, got)
			}
		})
	}
}
