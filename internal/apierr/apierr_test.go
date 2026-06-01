package apierr_test

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/apierr"
)

func TestLocalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		input       string
		wantPrefix  string // if non-empty, expected prefix after localization
		passThrough bool   // true: output must equal input unchanged
	}{
		{
			name:       "rate_limit envelope",
			input:      "API Error: rate_limit_exceeded: Retry after 60s",
			wantPrefix: "⏱️ Claude API 调用过于频繁",
		},
		{
			name:       "overloaded envelope",
			input:      "API Error: overloaded_error: Overloaded",
			wantPrefix: "🌊 Claude 服务当前负载较高",
		},
		{
			name:       "invalid api key envelope",
			input:      "API Error: invalid_api_key",
			wantPrefix: "🔑 Claude API 密钥无效",
		},
		{
			name:       "credit balance envelope",
			input:      "API Error: Your credit balance is too low",
			wantPrefix: "💳 Claude API 额度已用尽",
		},
		{
			name:       "context length envelope",
			input:      "API Error: prompt is too long: 200000 tokens > 200000",
			wantPrefix: "📏 对话上下文已超出模型上限",
		},
		{
			name:       "timeout envelope",
			input:      "API Error: request timed out",
			wantPrefix: "⏱️ 连接 Claude API 超时",
		},
		{
			name:       "permission envelope with Anthropic code",
			input:      "API Error: permission_error: forbidden",
			wantPrefix: "🚫 Claude 拒绝了本次请求",
		},
		{
			name:        "git tool output with 'forbidden' is NOT mis-localized",
			input:       "Error: git push: remote rejected — forbidden by branch protection",
			passThrough: true,
		},
		{
			name:        "non-envelope prose containing rate limit passes through",
			input:       "Sure — here is how rate limits work in HTTP…",
			passThrough: true,
		},
		{
			name:        "empty string passes through",
			input:       "",
			passThrough: true,
		},
		{
			name:        "regular assistant reply passes through",
			input:       "Here is the analysis you requested.",
			passThrough: true,
		},
		{
			name:       "error colon prefix with rate_limit",
			input:      "Error: rate_limit_exceeded",
			wantPrefix: "⏱️ Claude API 调用过于频繁",
		},
		{
			name:        "long non-envelope reply passes through (prefix heuristic)",
			input:       strings.Repeat("This is a long normal reply. ", 500),
			passThrough: true,
		},
		// API-error envelope (exit 0) that cron success path would send to IM
		// if not localized — regression guard for ARCH-1.
		{
			name:       "cron success path API error envelope localized",
			input:      "API Error: overloaded_error: The API is temporarily overloaded (request_id: req_abc123, host: api-internal.anthropic.com)",
			wantPrefix: "🌊 Claude 服务当前负载较高",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := apierr.Localize(tc.input)
			if tc.passThrough {
				if got != tc.input {
					t.Errorf("input should pass through unchanged\ninput = %q\ngot   = %q", tc.input, got)
				}
				return
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("expected prefix %q, got %q", tc.wantPrefix, got)
			}
			// Privacy contract: the raw error MUST NOT be appended to the
			// friendly reply. Operators retain it via slog.Warn only.
			if strings.Contains(got, "原始错误") {
				t.Errorf("localized output must not embed raw error text:\n%q", got)
			}
		})
	}
}
