package cron

// R20260531070014-ARCH-1 regression guard: cron success path must localize
// API-error envelopes before delivering to IM. Claude -p can exit 0 while
// result.Text is a raw API-error envelope containing request IDs, internal
// hostnames, and potentially leaked credentials. The success path at
// scheduler_run.go previously called formatCronNotice(sanitiseRunResult(text))
// without apierr.Localize, bypassing the privacy protection that dispatch's
// localizeAPIError provides on the IM hot path.

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/apierr"
)

// TestCronNotice_APIErrorEnvelopeIsLocalized verifies that the pipeline
// apierr.Localize(sanitiseRunResult(text)) used at line 1595 transforms
// API-error envelopes into user-facing Chinese messages and does not leak
// internal infrastructure details (request IDs, hostnames).
func TestCronNotice_APIErrorEnvelopeIsLocalized(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		resultText   string
		wantContains string // substring the final IM notice must contain
		mustNotHave  string // sensitive substring that must NOT appear in notice
	}{
		{
			name:         "overloaded envelope with request_id leaks nothing",
			resultText:   "API Error: overloaded_error: The API is temporarily overloaded (request_id: req_abc123xyz, host: api-internal.anthropic.com)",
			wantContains: "Claude 服务当前负载较高",
			mustNotHave:  "req_abc123xyz",
		},
		{
			name:         "rate_limit envelope with internal details sanitized",
			resultText:   "API Error: rate_limit_exceeded: Too many tokens (request_id: req_zzz999, retry_after: 60)",
			wantContains: "Claude API 调用过于频繁",
			mustNotHave:  "req_zzz999",
		},
		{
			name:         "invalid_api_key envelope hides key context",
			resultText:   "API Error: invalid_api_key: The provided API key sk-ant-xxx is invalid",
			wantContains: "Claude API 密钥无效",
			mustNotHave:  "sk-ant-xxx",
		},
		{
			name:         "normal result text passes through localize unchanged",
			resultText:   "Found 3 open PRs. No critical issues detected.",
			wantContains: "Found 3 open PRs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Replicate the pipeline from scheduler_run.go line 1595:
			//   apierr.Localize(sanitiseRunResult(result.Text))
			sanitised := sanitiseRunResult(tc.resultText)
			localized := apierr.Localize(sanitised)
			notice := formatCronNotice("test-job", localized)

			if !strings.Contains(notice, tc.wantContains) {
				t.Errorf("notice does not contain expected text %q\ngot: %q", tc.wantContains, notice)
			}
			if tc.mustNotHave != "" && strings.Contains(notice, tc.mustNotHave) {
				t.Errorf("notice leaks sensitive string %q that must be hidden\ngot: %q", tc.mustNotHave, notice)
			}
		})
	}
}

// TestCronNotice_LocalizeBeforeSanitise_OrderMatters verifies that sanitise
// must come before localize (privacy first): if a large API-error envelope
// were truncated by sanitiseRunResult, the shortened form must still be
// recognized as an envelope and localized rather than passed through raw.
func TestCronNotice_LocalizeBeforeSanitise_OrderMatters(t *testing.T) {
	t.Parallel()
	// Build a result where the envelope marker is in the first 64 bytes so
	// it survives sanitiseRunResult truncation; the tail contains the
	// sensitive data that should be stripped by localize.
	// Format: "API Error: rate_limit_exceeded: <large filler with sensitive data>"
	sensitive := "request_id: req_sensitive_abc123, host: prod-internal.example.com"
	long := "API Error: rate_limit_exceeded: " + sensitive + strings.Repeat(" filler", 200)
	sanitised := sanitiseRunResult(long)
	localized := apierr.Localize(sanitised)

	if strings.Contains(localized, "req_sensitive_abc123") {
		t.Errorf("sensitive request_id leaked through localize pipeline: %q", localized)
	}
	if !strings.Contains(localized, "Claude API 调用过于频繁") {
		t.Errorf("localized output should be rate-limit message, got: %q", localized)
	}
}
