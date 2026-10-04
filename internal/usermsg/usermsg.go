// Package usermsg maps internal sentinel errors from session/cli/shim onto
// short, user-facing Chinese messages suitable for IM replies and
// dashboard send_ack payloads. ForSendError is the single registration
// point for a new sentinel; callers needing path-specific phrasing wrap it.
//
// Sentinel→Code matching lives in classify.go (the ONLY file importing
// internal/cli + internal/session); the Code→text table here has no such
// dependency so it can move to internal/i18n (#631).
package usermsg

import (
	"fmt"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

// codeText is the presentation-layer table: Code → short Chinese label.
// CodeUnknown is intentionally absent so a missing-row bug surfaces via the
// ForSendError fallback rather than an empty string; see textForCode.
var codeText = map[Code]string{
	CodeMaxProcs:           "当前处理已满，请稍后重试。",
	CodeMaxExemptSessions:  "长时会话（planner/cron）已满，请联系管理员。",
	CodeNoCLIWrapper:       "会话后端未配置，请联系管理员。",
	CodeSessionAsleep:      "会话已休眠，请重新发送消息以唤醒。",
	CodeCronAsleep:         "定时任务会话已休眠，下一次触发会自动唤醒。",
	CodeTimeout:            "处理超时，请简化任务后重试。",
	CodeProcessExited:      "进程意外退出，请重新发送消息，系统会自动重启会话。",
	CodeAbortedByUrgent:    "上一条消息已被 /urgent 打断，请在当前任务完成后重发。",
	CodeReconnectedUnknown: "系统已重启，处理状态未知，请查看历史记录或重发。",
	CodeSessionReset:       "会话已重置。",
	CodeTooManyPending:     "当前会话排队已满，请稍候或使用 /stop 取消。",
	CodeProcessBusy:        "当前会话正在处理上一条消息，请稍候再发。",
	CodeMessageTooLarge:    "消息内容过大，请缩短后重试。",
	CodeRestarting:         "系统正在重启，请稍后重试。",
}

// genericRetryHint is the text for CodeUnknown and any unmapped Code.
const genericRetryHint = "处理失败，请发送 /new 重置后重试。"

// textForCode returns the Chinese label for c, or genericRetryHint.
func textForCode(c Code) string {
	if s, ok := codeText[c]; ok {
		return s
	}
	return genericRetryHint
}

// ForSendError returns a short Chinese label describing err for end-user
// display; "" when err is nil. Unknown errors collapse to a generic retry
// hint (operators see the raw error in logs). Wrapping details (paths,
// keys, goroutine IDs) are dropped so the result can go straight to a
// browser or IM channel. ErrNoActiveProcess on a cron-namespace key gets
// the "定时任务会话已休眠" phrasing; an empty key takes the regular one.
func ForSendError(err error, key string) string {
	if err == nil {
		return ""
	}
	return textForCode(classify(err, key))
}

// Config keys the timeout messages point operators at.
const (
	noOutputTimeoutKey = "session.watchdog.no_output_timeout"
	totalTimeoutKey    = "session.watchdog.total_timeout"
)

// maxToolNameRunes caps the tool label in a timeout message; ACP tool titles
// can be whole command lines.
const maxToolNameRunes = 60

// UserMessage maps err to a user-facing Chinese label. The watchdog timeouts
// get their own lines: the budget that ran out (the one the error carries,
// else the configured one), whether a tool or the model went silent, and the
// config key that raises it. Plain text without emoji so each surface owns
// its presentation; callers without per-session timeouts (dashboard WS
// send_ack) use ForSendError. A zero/negative duration renders as "未知".
func UserMessage(err error, key string, noOutputTimeout, totalTimeout time.Duration) string {
	switch {
	case isNoOutputTimeout(err):
		tool, elapsed, applied := silentTool(err)
		if applied > 0 {
			noOutputTimeout = applied
		}
		budget := textutil.FormatChineseDuration(noOutputTimeout)
		if tool != "" {
			return fmt.Sprintf("处理超时：工具 %s 已运行 %s，期间连续 %s 无任何输出，本轮已终止。长任务可调大 %s。",
				textutil.TruncateRunes(tool, maxToolNameRunes), textutil.FormatChineseDuration(elapsed), budget, noOutputTimeoutKey)
		}
		return fmt.Sprintf("处理超时：模型连续 %s 无输出（可能在生成超长回复，或 CLI 卡住），本轮已终止。可重试，或调大 %s。",
			budget, noOutputTimeoutKey)
	case isTotalTimeout(err):
		return fmt.Sprintf("处理超时（总耗时超过 %s），本轮已终止。请拆分为更小的任务，或调大 %s。",
			textutil.FormatChineseDuration(totalTimeout), totalTimeoutKey)
	default:
		return ForSendError(err, key)
	}
}
