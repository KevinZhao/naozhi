package dispatch

import (
	"context"
	"time"
)

// NotifyKind classifies the reason a caller wants a notify-ctx detached
// from its parent. The string form is operator-readable and shows up in
// the comment annotations at each call site so a future grep for the
// kind string finds every "why detached" branch.
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
	// the card post (R218-GO-1).
	NotifyKindAskQuestionCard NotifyKind = "ask_question_card"

	// NotifyKindTodoMessage — TodoWrite snapshot dispatch outlives the
	// turn so a near-deadline turn can still post its checklist
	// (R236-GO-1).
	NotifyKindTodoMessage NotifyKind = "todo_message"
)

// NotifyCtx returns a fresh ctx detached from parent, bounded by timeout.
// All four dispatch detached-reply sites converge here so the
// "fresh-Background" decision is made in one place rather than copy-pasted.
//
// R247-ARCH-10 / R244-ARCH-2 / R246-ARCH-17 (#632): pre-fix the same
// `context.WithTimeout(context.Background(), <reply-timeout>)` pattern
// repeated across panic recovery, shutdown reply, ask_question card
// dispatch, and TodoWrite dispatch — four sites with subtly different
// timeout constants and four different rationale comments. A future
// drift (e.g. someone tightening shutdownReplyTimeout) had to find every
// site to stay consistent. The factory keeps the caller's "kind" tag
// for log / breadcrumb purposes without changing observable behaviour;
// the parent argument is taken to make future "should we propagate
// deadlines" decisions a single edit, but is intentionally ignored
// today (the whole point of these sites is to detach).
//
// Returns the fresh ctx and a cancel func; callers MUST defer cancel
// or leak the timer goroutine.
func NotifyCtx(parent context.Context, kind NotifyKind, timeout time.Duration) (context.Context, context.CancelFunc) {
	// parent is intentionally unused for now. Keeping it in the signature
	// reserves room for future "best-effort propagate trace ID / cancel
	// on outer-shutdown via a longer fallback" without churning every call
	// site. Reference it once so a static-analysis pass that flags
	// unused params finds nothing to complain about.
	_ = parent
	_ = kind
	return context.WithTimeout(context.Background(), timeout)
}
