package cron

import (
	"strings"
	"testing"
	"time"
)

// The pure secret-redactor table tests live in internal/textutil
// (secrets_test.go). What remains here is the cron-specific integration
// coverage: the scheduler sanitise paths must scrub secrets before
// persistence / WS broadcast / log-injection passes.

// TestSanitiseRunErrMsg_Redacts: sanitiseRunErrMsg must scrub well-known
// secret prefixes and network addresses so a leaked token or topology in an
// error string (LastError) never lands on disk or the dashboard WS broadcast.
// [R20260531-SEC-8, R20260613-LOGIC-1].
func TestSanitiseRunErrMsg_Redacts(t *testing.T) {
	cases := []struct{ in, gone, have string }{
		{"exec failed: auth header sk-ant-api03-abcdef0123456789 rejected", "sk-ant-", "[REDACTED]"},
		{"git push denied with ghp_abcdef0123456789 token", "ghp_", "[REDACTED]"},
		{"session error: dial tcp 10.0.0.1:50051: connection refused", "10.0.0.1", "[redacted-addr]"},
	}
	for _, tc := range cases {
		got := sanitiseRunErrMsg(tc.in)
		if strings.Contains(got, tc.gone) {
			t.Errorf("sanitiseRunErrMsg left %q intact for %q → %q", tc.gone, tc.in, got)
		}
		if !strings.Contains(got, tc.have) {
			t.Errorf("sanitiseRunErrMsg did not insert %q for %q → %q", tc.have, tc.in, got)
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

// TestRecordTerminalResult_ErrMsgSecretRedacted is the integration guard for
// R090135-GO-2: a token (sk-ant-/ghp_/AKIA…) embedded in the errMsg argument
// must not survive into Job.LastError → cron_jobs.json. Before the fix only
// the result branch called textutil.RedactSecrets; the errMsg branch only called
// redactPathsInCronError (path-redaction only), leaving tokens intact.
func TestRecordTerminalResult_ErrMsgSecretRedacted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: dir + "/cron.json",
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{
		ID:       "errredc01",
		Schedule: "@every 1h",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true,
	}
	s.putJobForTest(j)

	const rawToken = "sk-ant-api03-abcdef0123456789abcdef"
	errInput := "session error: header " + rawToken + " rejected"

	_, gotErrMsg, _ := s.recordTerminalResult(j.ID, runOutcome{errMsg: errInput, errClass: ErrClassSessionError, state: RunStateFailed}, time.Now())
	if strings.Contains(gotErrMsg, rawToken) {
		t.Errorf("recordTerminalResult returned errMsg still contains token: %q", gotErrMsg)
	}
	if !strings.Contains(gotErrMsg, "[REDACTED]") {
		t.Errorf("recordTerminalResult returned errMsg missing [REDACTED]: %q", gotErrMsg)
	}
	// Also assert the persisted Job field is clean.
	lastErr := s.jobForTest(t, j.ID).LastError
	if strings.Contains(lastErr, rawToken) {
		t.Errorf("Job.LastError still contains token after recordTerminalResult: %q", lastErr)
	}
}
