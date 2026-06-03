package apierr_test

import (
	"context"
	"log/slog"
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
			name:       "network error envelope localized",
			input:      "API Error: network error while contacting api.anthropic.com",
			wantPrefix: "🌐 与 Claude API 的网络连接出现问题",
		},
		{
			name:       "connection refused envelope localized",
			input:      "API Error: connection refused",
			wantPrefix: "🌐 与 Claude API 的网络连接出现问题",
		},
		{
			name:        "git tool output with 'forbidden' is NOT mis-localized",
			input:       "Error: git push: remote rejected — forbidden by branch protection",
			passThrough: true,
		},
		// GO-1 regression: tool stderr starting with "Error:" must NOT be
		// treated as an API-error envelope, even when the body contains
		// keywords like timeout/connection/network/authentication.
		{
			name:        "ssh connection timeout stderr passes through",
			input:       "Error: ssh: connect to host github.com port 22: Connection timed out",
			passThrough: true,
		},
		{
			name:        "git network error stderr passes through",
			input:       "Error: git clone: network error: connection reset",
			passThrough: true,
		},
		{
			name:        "git authentication failure stderr passes through",
			input:       "Error: authentication failed for 'https://github.com/user/repo'",
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
		// GO-1: a bare "Error:" prefix is no longer treated as an API
		// envelope. Anthropic rate-limit envelopes always carry the
		// canonical "API Error" prefix, which is still localized below.
		{
			name:        "error colon prefix with rate_limit passes through",
			input:       "Error: rate_limit_exceeded",
			passThrough: true,
		},
		{
			name:       "anthropic api error prefix rate_limit localized",
			input:      "Anthropic API Error: rate_limit_exceeded",
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
		// R20260601-GO-2: "authentication" narrowed to "authentication_error".
		// A proxy error merely containing the word "authentication" must NOT
		// be mis-localized as an API-key failure. It is still an "API Error:"
		// envelope, so it falls through to the generic default message (per
		// the SEC-2 hardening that no longer forwards the raw envelope) — the
		// key assertion is that it is NOT the 🔑 key-failure copy.
		{
			name:       "proxy authentication message is NOT mis-localized as key failure (R20260601-GO-2)",
			input:      "API Error: authentication required by proxy",
			wantPrefix: "⚠️ Claude API 返回了一个未识别的错误",
		},
		{
			name:       "authentication_error code is still localized (R20260601-GO-2)",
			input:      "API Error: authentication_error: invalid x-api-key",
			wantPrefix: "🔑 Claude API 密钥无效",
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

// capturingHandler is a minimal slog.Handler that collects all logged
// key-value pairs so tests can assert on log content without involving
// real log output.
type capturingHandler struct {
	attrs []slog.Attr
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingHandler) WithGroup(_ string) slog.Handler              { return h }
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := &capturingHandler{attrs: append(h.attrs[:len(h.attrs):len(h.attrs)], attrs...)}
	return n
}
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}

// TestLocalizeDoesNotLogRawSecret verifies that slog.Warn inside Localize
// never records the raw envelope text, which may contain sk-ant- API keys,
// request_ids, or internal hostnames (R20260601-SEC-1).
func TestLocalizeDoesNotLogRawSecret(t *testing.T) {
	// No t.Parallel: this test installs a per-test handler via
	// slog.SetDefault, while TestLocalize's parallel subtests call
	// apierr.Localize -> slog.Warn, which reads slog.Default(). With
	// t.Parallel the SetDefault write races those reads under -race.

	// Install a capturing handler for the duration of this test.
	handler := &capturingHandler{}
	orig := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(orig) })

	secret := "sk-ant-api03-supersecretkey12345"
	envelope := "API Error: invalid_api_key - " + secret + " (request_id: req_abc123, host: api-internal.anthropic.com)"

	got := apierr.Localize(envelope)

	// Return value must be the friendly message, not the raw envelope.
	if !strings.HasPrefix(got, "🔑") {
		t.Fatalf("expected friendly message, got %q", got)
	}

	// None of the logged attribute values must contain the secret or raw envelope.
	for _, attr := range handler.attrs {
		v := attr.Value.String()
		if strings.Contains(v, secret) {
			t.Errorf("slog attribute %q contains secret key: %q", attr.Key, v)
		}
		if strings.Contains(v, "request_id") {
			t.Errorf("slog attribute %q contains request_id: %q", attr.Key, v)
		}
		if strings.Contains(v, "api-internal") {
			t.Errorf("slog attribute %q contains internal hostname: %q", attr.Key, v)
		}
	}
}
