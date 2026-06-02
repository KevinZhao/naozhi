package cron

import (
	"strings"
	"testing"
)

// The pure secret-redactor table tests now live in internal/textutil
// (secrets_test.go) since the scan logic moved there in
// R20260602-091302-ARCH-1 (#1571). What remains here is the cron-specific
// integration coverage: the scheduler sanitise paths must scrub secrets
// before persistence / WS broadcast / log-injection passes.

// TestSanitiseRunErrMsg_RedactsSecrets is the integration coverage for the
// error path: sanitiseRunErrMsg must scrub well-known secret prefixes so a
// leaked token in an error string (LastError) never lands on disk or the
// dashboard WS broadcast. [R20260531-SEC-8].
func TestSanitiseRunErrMsg_RedactsSecrets(t *testing.T) {
	cases := []string{
		"exec failed: auth header sk-ant-api03-abcdef0123456789 rejected",
		"git push denied with ghp_abcdef0123456789 token",
	}
	for _, in := range cases {
		got := sanitiseRunErrMsg(in)
		if strings.Contains(got, "sk-ant-") || strings.Contains(got, "ghp_") {
			t.Errorf("sanitiseRunErrMsg left token intact for %q → %q", in, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("sanitiseRunErrMsg did not insert [REDACTED] for %q → %q", in, got)
		}
	}
}

// TestSanitiseRunResult_RedactsSecrets is the integration coverage: the
// production sanitiseRunResult path (used by skipPersist branches and
// recordTerminalResult) must scrub secrets before the SanitizeForLog
// log-injection pass. R234-SEC-7 (#1006).
func TestSanitiseRunResult_RedactsSecrets(t *testing.T) {
	in := "claude said: sk-ant-api03-abcdef0123456789 is your key"
	got := sanitiseRunResult(in)
	if strings.Contains(got, "sk-ant-") {
		t.Errorf("sanitiseRunResult left token intact: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("sanitiseRunResult did not insert [REDACTED]: %q", got)
	}
}

// TestRedactSecrets_AliasDelegates confirms the deprecated cron.RedactSecrets
// alias still scrubs (it now delegates to textutil.RedactSecrets). #1571.
func TestRedactSecrets_AliasDelegates(t *testing.T) {
	got := RedactSecrets("token sk-ant-api03-abcdef0123456789 here")
	if got != "token [REDACTED] here" {
		t.Errorf("cron.RedactSecrets alias diverged: %q", got)
	}
}
