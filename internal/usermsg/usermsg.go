// Package usermsg maps internal sentinel errors from session/cli/shim onto
// short, user-facing Chinese messages suitable for IM replies and
// dashboard send_ack payloads.
//
// Both delivery paths (IM via dispatch, WebSocket via server) used to keep
// nearly-identical switch statements with cross-package "keep in sync"
// comments. R226-CR-9 collapses the shared cases into ForSendError so a
// new sentinel only needs to be registered once. Callers that need
// path-specific phrasing (e.g. dispatch embeds the configured
// no-output / total timeout durations in Chinese) wrap this helper:
//
//	if msg, ok := dispatchSpecific(err); ok { return msg }
//	return usermsg.ForSendError(err)
package usermsg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/textutil"
)

// ForSendError returns a short Chinese label describing err for end-user
// display. Returns "" when err is nil. Unknown errors collapse to a
// generic retry hint; operators should still see the raw error in logs.
//
// The function intentionally drops wrapping details (paths, keys,
// goroutine IDs) so that callers can pass the result straight to a
// browser or IM channel without re-sanitising.
//
// CronKey-aware: ErrNoActiveProcess on a cron-namespace key returns the
// "定时任务会话已休眠" phrasing instead of the user-typeable /new hint
// (R218-CR-2). Callers that already know the key kind can pass it; an
// empty key takes the regular phrasing.
func ForSendError(err error, key string) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, session.ErrMaxProcs):
		return "当前处理已满，请稍后重试。"
	case errors.Is(err, session.ErrMaxExemptSessions):
		return "长时会话（planner/cron）已满，请联系管理员。"
	case errors.Is(err, session.ErrNoCLIWrapper):
		return "会话后端未配置，请联系管理员。"
	case errors.Is(err, session.ErrNoActiveProcess):
		if session.IsCronKey(key) {
			return "定时任务会话已休眠，下一次触发会自动唤醒。"
		}
		return "会话已休眠，请重新发送消息以唤醒。"
	case errors.Is(err, cli.ErrNoOutputTimeout), errors.Is(err, cli.ErrTotalTimeout):
		return "处理超时，请简化任务后重试。"
	case errors.Is(err, cli.ErrProcessExited):
		return "进程意外退出，请重新发送消息，系统会自动重启会话。"
	case errors.Is(err, cli.ErrAbortedByUrgent):
		return "上一条消息已被 /urgent 打断，请在当前任务完成后重发。"
	case errors.Is(err, cli.ErrReconnectedUnknown):
		return "系统已重启，处理状态未知，请查看历史记录或重发。"
	case errors.Is(err, cli.ErrSessionReset):
		return "会话已重置。"
	case errors.Is(err, cli.ErrTooManyPending):
		return "当前会话排队已满，请稍候或使用 /stop 取消。"
	case errors.Is(err, cli.ErrProcessBusy):
		return "当前会话正在处理上一条消息，请稍候再发。"
	case errors.Is(err, cli.ErrMessageTooLarge):
		return "消息内容过大，请缩短后重试。"
	case errors.Is(err, cli.ErrOrphanedSlot):
		return "处理超时，请稍后重试。"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "系统正在重启，请稍后重试。"
	default:
		return "处理失败，请发送 /new 重置后重试。"
	}
}

// UserMessage maps err to a user-facing Chinese label, with timeout-aware
// specialisation: cli.ErrNoOutputTimeout / cli.ErrTotalTimeout render the
// configured per-session no-output / total durations using
// textutil.FormatChineseDuration so the user sees the actual budget
// rather than a generic "处理超时" line.
//
// Callers that have no per-session timeouts (dashboard send_ack on the
// WS path) should keep using ForSendError directly — its collapsed
// timeout branch yields the generic phrasing. Callers with timeouts
// (IM dispatch) consume this helper and decorate the result if they
// want a leading emoji or other channel-specific styling: the helper
// returns plain text without emoji so each surface owns its own
// presentation. R226-CR-9 collapsed the duplicated sentinel switch
// onto ForSendError; R249-DISPATCH-1 (#419) extracts the timeout
// specialisation here so the dispatch send path no longer keeps a
// parallel switch with cross-package "keep in sync" comments.
//
// noOutputTimeout / totalTimeout are zero-safe: a zero/negative duration
// renders as "未知" via textutil.FormatChineseDuration. Production callers
// always pass non-zero values from DispatcherConfig.
func UserMessage(err error, key string, noOutputTimeout, totalTimeout time.Duration) string {
	switch {
	case errors.Is(err, cli.ErrNoOutputTimeout):
		return fmt.Sprintf("处理超时（%s 无输出），请简化任务后重试。", textutil.FormatChineseDuration(noOutputTimeout))
	case errors.Is(err, cli.ErrTotalTimeout):
		return fmt.Sprintf("处理超时（总耗时超过 %s），请拆分为更小的任务。", textutil.FormatChineseDuration(totalTimeout))
	default:
		return ForSendError(err, key)
	}
}
