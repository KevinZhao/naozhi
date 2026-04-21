package dispatch

import (
	"log/slog"
	"strings"
)

// envelopePrefixMaxLen is the maximum number of leading bytes we lowercase to
// test whether a result string looks like an API-error envelope. Keeping this
// small avoids an O(N) copy on every normal assistant reply (which may be tens
// of KB) just to discover the text is not an error.
const envelopePrefixMaxLen = 64

// localizeAPIError rewrites common Claude / Anthropic API error strings that
// surface verbatim in the CLI result into friendlier Chinese guidance for IM
// users. When no known pattern matches, the original text is returned
// unchanged — non-error results always pass through.
//
// Detection is deliberately conservative: we only transform strings that
// look like top-level API error envelopes (start with "API Error" or are
// short, error-only payloads). This avoids mangling legitimate content that
// happens to contain a keyword like "rate limit" in prose.
//
// Privacy: the raw error is NOT appended to the IM reply — it may contain
// internal infrastructure details (proxy URLs, request IDs, or, in the
// worst case, leaked credentials). The full text is logged at Warn so
// operators retain diagnostics without exposing them to end users.
func localizeAPIError(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}

	// Lowercase only the leading prefix for envelope detection — normal
	// assistant replies can be tens of KB; a full ToLower on every reply
	// just to test HasPrefix is a wasteful allocation on the IM hot path.
	prefix := trimmed
	if len(prefix) > envelopePrefixMaxLen {
		prefix = prefix[:envelopePrefixMaxLen]
	}
	lowerPrefix := strings.ToLower(prefix)
	isEnvelope := strings.HasPrefix(lowerPrefix, "api error") ||
		strings.HasPrefix(lowerPrefix, "error:") ||
		strings.HasPrefix(lowerPrefix, "anthropic api error")
	if !isEnvelope {
		return text
	}

	// Only lowercase the full body now that we know it is an error envelope.
	lower := strings.ToLower(trimmed)

	// Classify. If no known pattern matches, fall through to a generic
	// user-facing message — the raw error goes to the log, not IM.
	var friendly string
	switch {
	case strings.Contains(lower, "rate_limit") || strings.Contains(lower, "rate limit"):
		friendly = "⏱️ Claude API 调用过于频繁，请稍候一分钟再试。"
	case strings.Contains(lower, "overloaded"):
		friendly = "🌊 Claude 服务当前负载较高，请稍后重试。"
	case strings.Contains(lower, "invalid_api_key") || strings.Contains(lower, "authentication"):
		friendly = "🔑 Claude API 密钥无效或已过期，请联系管理员检查配置。"
	case strings.Contains(lower, "insufficient_quota") || strings.Contains(lower, "credit balance") || strings.Contains(lower, "billing"):
		friendly = "💳 Claude API 额度已用尽，请联系管理员充值后重试。"
	case strings.Contains(lower, "context_length") || strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "maximum context"):
		friendly = "📏 对话上下文已超出模型上限，请发送 /new 开启新会话。"
	// Narrower match: require the canonical Anthropic error codes so a tool
	// output like `git push: forbidden` forwarded through the CLI does not
	// collapse into the generic "permission / 内容策略" branch.
	case strings.Contains(lower, "permission_error") || strings.Contains(lower, "permission_denied") || strings.Contains(lower, "request_forbidden"):
		friendly = "🚫 Claude 拒绝了本次请求（权限或内容策略），请调整后重试。"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		friendly = "⏱️ 连接 Claude API 超时，请稍后重试。"
	case strings.Contains(lower, "network") || strings.Contains(lower, "connection"):
		friendly = "🌐 与 Claude API 的网络连接出现问题，请稍后重试。"
	default:
		return text
	}

	// Keep the raw envelope on the operator side only — never forward it
	// to IM users. trimmed may contain sensitive substrings (request IDs,
	// internal hostnames, upstream error headers).
	slog.Warn("claude api error envelope localized", "raw", trimmed)
	return friendly
}
