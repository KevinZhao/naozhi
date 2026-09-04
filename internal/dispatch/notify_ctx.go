package dispatch

import (
	"context"
	"time"
)

// NotifyKind classifies why a caller wants a notify-ctx detached from its
// parent; the string form is operator-readable.
type NotifyKind string

const (
	// NotifyKindShutdown — parent ctx is already Done (system shutdown /
	// SIGTERM); reply must still try to land the "正在重启" notice on a
	// short fresh budget. shutdownReplyTimeout governs the cap.
	NotifyKindShutdown NotifyKind = "shutdown"

	// NotifyKindOwnerLoopPanic — owner-loop panic recovery wants to
	// surface a user-facing error reply even though the turn ctx may
	// have been cancelled by the panic-driven defer chain.
	NotifyKindOwnerLoopPanic NotifyKind = "owner_loop_panic"

	// NotifyKindAskQuestionCard — AskUserQuestion card dispatch outlives
	// the originating turn so the question appears even if /new races
	// the card post.
	NotifyKindAskQuestionCard NotifyKind = "ask_question_card"

	// NotifyKindTodoMessage — TodoWrite snapshot dispatch outlives the
	// turn so a near-deadline turn can still post its checklist.
	NotifyKindTodoMessage NotifyKind = "todo_message"
)

// NotifyCtx returns a fresh ctx detached from parent, bounded by timeout.
// All dispatch detached-reply sites converge here so the "fresh-Background"
// decision lives in one place (#632). parent and kind are intentionally
// ignored today: parent reserves room for future deadline propagation, kind
// tags the call site for log / grep purposes. Callers MUST defer cancel.
func NotifyCtx(parent context.Context, kind NotifyKind, timeout time.Duration) (context.Context, context.CancelFunc) {
	_ = parent
	_ = kind
	return context.WithTimeout(context.Background(), timeout)
}
