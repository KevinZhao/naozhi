package dispatch

import (
	"context"
	"testing"
)

// TestSuccessReplyCtx_SwapsWhenStopCtxCanceled is the #2316 regression guard.
//
// A passthrough turn's ctx is bound to d.stopCtx (mergeStopAndValues) so an
// in-flight turn can observe SIGTERM. The success-reply section of
// sendAndReply (waitReady / EditMessage / SendSplitReply / outbound image
// Reply) delivers the generated answer. If a graceful shutdown cancels the
// merged ctx in the race window between d.caps.Send returning and the reply
// being delivered, the section must NOT deliver on the Done ctx — that aborts
// every http request and silently drops the answer the user waited for.
//
// The fix wraps the success-reply delivery in resolveReplyCtx, exactly like
// the two error-reply paths and the panic / card / todo paths. This test pins
// the load-bearing property: feeding a canceled merged stop-ctx through
// resolveReplyCtx yields a live ctx with a real shutdown budget, so the
// downstream delivery calls run on a usable ctx.
func TestSuccessReplyCtx_SwapsWhenStopCtxCanceled(t *testing.T) {
	t.Parallel()

	// Reconstruct the production wiring: the turn ctx is the merge of a
	// service stopCtx (cancel source) and a webhook values ctx.
	stopCtx, stopCancel := context.WithCancel(context.Background())
	valuesCtx, valuesCancel := context.WithCancel(context.Background())
	defer valuesCancel()

	turnCtx := mergeStopAndValues(stopCtx, valuesCtx)

	// SIGTERM during the race window: the cancel source fires.
	stopCancel()
	<-turnCtx.Done()
	if turnCtx.Err() == nil {
		t.Fatal("precondition: merged turn ctx must be Done after stopCancel")
	}

	// The success-reply section now resolves a delivery ctx.
	replyCtx, cleanup := resolveReplyCtx(turnCtx)
	if cleanup != nil {
		defer cleanup()
	}

	if replyCtx.Err() != nil {
		t.Fatalf("#2316: success-reply ctx still Done (%v) — answer would be dropped on shutdown", replyCtx.Err())
	}
	if _, ok := replyCtx.Deadline(); !ok {
		t.Fatal("#2316: swapped reply ctx has no shutdown budget deadline")
	}
}

// TestSuccessReplyCtx_HappyPathNoSwap pins the negative case: on a live turn
// ctx the success-reply section must pass it through unchanged (nil cleanup),
// paying nothing extra on the hot path.
func TestSuccessReplyCtx_HappyPathNoSwap(t *testing.T) {
	t.Parallel()

	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	valuesCtx, valuesCancel := context.WithCancel(context.Background())
	defer valuesCancel()

	turnCtx := mergeStopAndValues(stopCtx, valuesCtx)

	replyCtx, cleanup := resolveReplyCtx(turnCtx)
	if cleanup != nil {
		t.Fatal("happy path: cleanup must be nil (no swap on a live ctx)")
	}
	if replyCtx != turnCtx {
		t.Fatal("happy path: live turn ctx must pass through unchanged")
	}
}
