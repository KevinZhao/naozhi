// Package apierr provides a leaf-level helper for detecting and localizing
// Claude / Anthropic API error envelopes that surface verbatim in CLI output.
//
// It is intentionally a leaf package (no internal imports) so both
// internal/dispatch and internal/cron can import it without creating an
// import cycle.
package apierr

import (
	"log/slog"
	"strings"
)

// Kind is the category of an API error, independent of its wording.
type Kind int

const (
	// KindUnrecognized is an envelope that matched no category.
	KindUnrecognized Kind = iota
	KindRateLimit
	KindOverloaded
	KindAuth
	KindQuota
	KindContextLength
	KindPermission
	KindTimeout
	KindNetwork
)

// kinds holds each Kind's friendly text and its slog "category" label, a
// short non-sensitive name so logs never carry the raw error text (sk-ant-
// keys, request_ids, internal hostnames).
var kinds = [...]struct{ friendly, category string }{
	KindUnrecognized:  {"⚠️ Claude API 返回了一个未识别的错误，已记录日志，请联系管理员。", "unknown"},
	KindRateLimit:     {"⏱️ Claude API 调用过于频繁，请稍候一分钟再试。", "rate_limit"},
	KindOverloaded:    {"🌊 Claude 服务当前负载较高，请稍后重试。", "overloaded"},
	KindAuth:          {"🔑 Claude API 密钥无效或已过期，请联系管理员检查配置。", "invalid_api_key"},
	KindQuota:         {"💳 Claude API 额度已用尽，请联系管理员充值后重试。", "insufficient_quota"},
	KindContextLength: {"📏 对话上下文已超出模型上限，请发送 /new 开启新会话。", "context_length"},
	KindPermission:    {"🚫 Claude 拒绝了本次请求（权限或内容策略），请调整后重试。", "permission_error"},
	KindTimeout:       {"⏱️ 连接 Claude API 超时，请稍后重试。", "timeout"},
	KindNetwork:       {"🌐 与 Claude API 的网络连接出现问题，请稍后重试。", "network"},
}

// envelopePrefixScanBytes bounds how many leading bytes are lowercased when
// probing for an API-error envelope, avoiding an O(N) copy per normal reply.
const envelopePrefixScanBytes = 64

// Localize rewrites Claude / Anthropic API error envelopes that surface
// verbatim in CLI output into friendlier Chinese guidance for IM users.
// Non-envelope text (anything not starting with "API Error") passes through
// unchanged so prose mentioning "rate limit" is never mangled.
//
// Privacy: the raw error is never appended to the IM reply nor logged — it
// may contain proxy URLs, request IDs or leaked credentials.
func Localize(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !isEnvelope(trimmed) {
		return text
	}
	k, _ := classify(strings.ToLower(trimmed), true)
	logLocalized(k, len(trimmed))
	return kinds[k].friendly
}

// LocalizeError is Localize for text the caller already knows is an error
// (claude's is_error result), so it also rewrites a bare "Prompt is too long"
// or "Credit balance is too low" without the envelope prefix. Without the
// prefix only the unambiguous categories match; ok=false returns text as is.
func LocalizeError(text string) (localized string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text, false
	}
	k, ok := ClassifyError(trimmed)
	if !ok {
		return text, false
	}
	logLocalized(k, len(trimmed))
	return kinds[k].friendly, true
}

// ClassifyError is LocalizeError's category without its text, for a caller
// that words the error itself; ok=false where LocalizeError would pass the
// text through.
func ClassifyError(text string) (Kind, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return KindUnrecognized, false
	}
	return classify(strings.ToLower(trimmed), isEnvelope(trimmed))
}

// isEnvelope reports whether trimmed starts with an API-error prefix.
// Lowercases only the leading bytes; replies can be tens of KB.
func isEnvelope(trimmed string) bool {
	prefix := trimmed
	if len(prefix) > envelopePrefixScanBytes {
		prefix = prefix[:envelopePrefixScanBytes]
	}
	lowerPrefix := strings.ToLower(prefix)
	return strings.HasPrefix(lowerPrefix, "api error") ||
		strings.HasPrefix(lowerPrefix, "anthropic api error")
}

// classify picks the Kind of a lowercased error. An envelope always gets one
// (KindUnrecognized included); bare text skips the timeout and network
// categories, whose words are common in ordinary tool errors, and returns
// ok=false when nothing matched.
func classify(lower string, envelope bool) (Kind, bool) {
	switch {
	case strings.Contains(lower, "rate_limit") || strings.Contains(lower, "rate limit"):
		return KindRateLimit, true
	case strings.Contains(lower, "overloaded"):
		return KindOverloaded, true
	case strings.Contains(lower, "invalid_api_key") || strings.Contains(lower, "authentication_error"):
		return KindAuth, true
	case strings.Contains(lower, "insufficient_quota") || strings.Contains(lower, "credit balance") || strings.Contains(lower, "billing"):
		return KindQuota, true
	case strings.Contains(lower, "context_length") || strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "maximum context"):
		return KindContextLength, true
	// Require canonical Anthropic codes so tool output like
	// `git push: forbidden` does not land in the permission branch.
	case strings.Contains(lower, "permission_error") || strings.Contains(lower, "permission_denied") || strings.Contains(lower, "request_forbidden"):
		return KindPermission, true
	case !envelope:
		return KindUnrecognized, false
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return KindTimeout, true
	case strings.Contains(lower, "network") || strings.Contains(lower, "connection"):
		return KindNetwork, true
	default:
		return KindUnrecognized, true
	}
}

// logLocalized records a localization by category only; the raw error may
// contain keys or request_ids.
func logLocalized(k Kind, rawLen int) {
	slog.Warn("claude api error envelope localized",
		"category", kinds[k].category,
		"envelope_len", rawLen,
	)
}
