package cron

import (
	"strings"
	"testing"
)

// TestCronNotice_APIErrorEnvelopeIsLocalized: claude -p can exit 0 with a raw
// API-error envelope as its result (request IDs, internal hostnames, key
// fragments). The cron success notice must localize it before it reaches IM,
// including an envelope long enough to be truncated by sanitiseRunResult first.
func TestCronNotice_APIErrorEnvelopeIsLocalized(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		resultText   string
		wantContains string
		mustNotHave  string
	}{
		{
			name:         "overloaded",
			resultText:   "API Error: overloaded_error: The API is temporarily overloaded (request_id: req_abc123xyz, host: api-internal.anthropic.com)",
			wantContains: "Claude 服务当前负载较高",
			mustNotHave:  "req_abc123xyz",
		},
		{
			name:         "rate_limit",
			resultText:   "API Error: rate_limit_exceeded: Too many tokens (request_id: req_zzz999, retry_after: 60)",
			wantContains: "Claude API 调用过于频繁",
			mustNotHave:  "req_zzz999",
		},
		{
			name:         "invalid_api_key",
			resultText:   "API Error: invalid_api_key: The provided API key sk-ant-xxx is invalid",
			wantContains: "Claude API 密钥无效",
			mustNotHave:  "sk-ant-xxx",
		},
		{
			name:         "truncated envelope still localized",
			resultText:   "API Error: rate_limit_exceeded: request_id: req_sensitive_abc123, host: prod-internal.example.com" + strings.Repeat(" filler", 200),
			wantContains: "Claude API 调用过于频繁",
			mustNotHave:  "req_sensitive_abc123",
		},
		{
			name:         "normal text passes through",
			resultText:   "Found 3 open PRs. No critical issues detected.",
			wantContains: "Found 3 open PRs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notice := formatCronNotice("test-job", localizeNotice(tc.resultText))
			if !strings.Contains(notice, tc.wantContains) {
				t.Errorf("notice missing %q\ngot: %q", tc.wantContains, notice)
			}
			if tc.mustNotHave != "" && strings.Contains(notice, tc.mustNotHave) {
				t.Errorf("notice leaks %q\ngot: %q", tc.mustNotHave, notice)
			}
		})
	}
}
