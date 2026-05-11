// errors_usermsg.go maps internal sentinel errors from session/cli/shim into
// user-facing messages suitable for WebSocket `send_ack.error` payloads.
//
// The raw err.Error() can include paths, session keys, and internal handler
// names that are useful in server logs but should not be shipped to browsers —
// an authenticated-but-non-operator viewer should not learn filesystem layout
// or process identifiers from a failure response.
package server

import (
	"context"
	"errors"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// asyncErrorMessage returns a short Chinese user-facing label for err. It
// intentionally drops wrapping details (paths, keys, goroutine IDs) so that
// callers can pass the result straight to a browser. Unknown errors collapse
// to a generic retry hint; operators should still see the raw error in logs.
func asyncErrorMessage(err error) string {
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
		// Distinct from the generic "/new" hint — a reset won't help, the
		// only remedy is to shorten the message or downscale attachments.
		return "消息内容过大，请缩短后重试。"
	case errors.Is(err, cli.ErrOrphanedSlot):
		return "处理超时，请稍后重试。"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "系统正在重启，请稍后重试。"
	default:
		return "处理失败，请发送 /new 重置后重试。"
	}
}
