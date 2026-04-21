package dispatch

import (
	"strings"
	"testing"
)

func TestLocalizeAPIError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		input       string
		want        string // if non-empty, exact expected return
		wantPrefix  string // if non-empty, expected prefix
		shouldMatch bool   // true: output differs from input (was localized)
	}{
		{
			name:        "rate_limit envelope",
			input:       "API Error: rate_limit_exceeded: Retry after 60s",
			wantPrefix:  "⏱️ Claude API 调用过于频繁",
			shouldMatch: true,
		},
		{
			name:        "overloaded envelope",
			input:       "API Error: overloaded_error: Overloaded",
			wantPrefix:  "🌊 Claude 服务当前负载较高",
			shouldMatch: true,
		},
		{
			name:        "invalid api key envelope",
			input:       "API Error: invalid_api_key",
			wantPrefix:  "🔑 Claude API 密钥无效",
			shouldMatch: true,
		},
		{
			name:        "credit balance envelope",
			input:       "API Error: Your credit balance is too low",
			wantPrefix:  "💳 Claude API 额度已用尽",
			shouldMatch: true,
		},
		{
			name:        "context length envelope",
			input:       "API Error: prompt is too long: 200000 tokens > 200000",
			wantPrefix:  "📏 对话上下文已超出模型上限",
			shouldMatch: true,
		},
		{
			name:        "timeout envelope",
			input:       "API Error: request timed out",
			wantPrefix:  "⏱️ 连接 Claude API 超时",
			shouldMatch: true,
		},
		{
			name:        "permission envelope with Anthropic code",
			input:       "API Error: permission_error: forbidden",
			wantPrefix:  "🚫 Claude 拒绝了本次请求",
			shouldMatch: true,
		},
		{
			name:       "git tool output with 'forbidden' is NOT mis-localized",
			input:      "Error: git push: remote rejected — forbidden by branch protection",
			wantPrefix: "",
		},
		{
			name:       "non-envelope prose containing rate limit passes through",
			input:      "Sure — here is how rate limits work in HTTP…",
			wantPrefix: "",
		},
		{
			name:       "empty string passes through",
			input:      "",
			wantPrefix: "",
		},
		{
			name:       "regular assistant reply passes through",
			input:      "Here is the analysis you requested.",
			wantPrefix: "",
		},
		{
			name:        "error colon prefix with rate_limit",
			input:       "Error: rate_limit_exceeded",
			wantPrefix:  "⏱️ Claude API 调用过于频繁",
			shouldMatch: true,
		},
		{
			name:       "long non-envelope reply passes through (prefix heuristic)",
			input:      strings.Repeat("This is a long normal reply. ", 500),
			wantPrefix: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := localizeAPIError(tc.input)
			if tc.wantPrefix == "" {
				if got != tc.input {
					t.Errorf("input should pass through unchanged\ninput = %q\ngot   = %q", tc.input, got)
				}
				return
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("expected prefix %q, got %q", tc.wantPrefix, got)
			}
			if tc.shouldMatch {
				// Privacy contract: the raw error MUST NOT be appended to
				// the friendly reply (even the trimmed form). Operators
				// retain it via slog.Warn only.
				if strings.Contains(got, "原始错误") {
					t.Errorf("localized output must not embed raw error text:\n%q", got)
				}
			}
		})
	}
}
