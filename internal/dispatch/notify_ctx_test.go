package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNotifyCtx_DetachedFromParent pins the central invariant of #632:
// the returned ctx is NOT cancelled when parent is cancelled. Pre-fix
// each call site repeated `context.WithTimeout(context.Background(), ...)`
// with the rationale "parent is already Done"; post-fix the factory must
// preserve that detach behaviour so callers don't accidentally regress
// to a parent-derived ctx.
func TestNotifyCtx_DetachedFromParent(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel() // immediately Done

	notify, notifyCancel := NotifyCtx(parent, NotifyKindShutdown, 1*time.Second)
	defer notifyCancel()

	// notify must NOT inherit parent's cancellation. If it did,
	// `notify.Err() != nil` here would silently drop the user-facing
	// "正在重启" notice on every shutdown — the original bug R188-CONC-M1
	// was about exactly this regression.
	if err := notify.Err(); err != nil {
		t.Fatalf("notify.Err() = %v immediately after construction; ctx should be live", err)
	}

	// Wait a short interval well under the 1s timeout. If the factory
	// were inadvertently re-deriving from parent (cancelled), notify
	// would already be Done; if it were attaching parentCtx as a Cause,
	// the same would apply.
	select {
	case <-notify.Done():
		t.Fatalf("notify ctx Done() fired despite parent cancellation; want detached")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestNotifyCtx_TimeoutHonored — the factory still bounds the returned
// ctx by the caller-provided timeout. Without this assertion a future
// refactor that returned context.Background() unconditionally would
// silently allow leaking goroutines on every detached-reply site.
func TestNotifyCtx_TimeoutHonored(t *testing.T) {
	t.Parallel()

	notify, cancel := NotifyCtx(context.Background(), NotifyKindOwnerLoopPanic, 30*time.Millisecond)
	defer cancel()

	select {
	case <-notify.Done():
		// Expected.
		if !errors.Is(notify.Err(), context.DeadlineExceeded) {
			t.Fatalf("notify.Err() = %v, want DeadlineExceeded", notify.Err())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("notify ctx did not honor 30ms timeout")
	}
}

// TestNotifyCtx_NilParentSafe — call sites pass nil parent today
// (panic-recovery + ask_question card both pre-detach completely).
// The factory must not panic on nil parent, otherwise ownerLoop panic
// recovery would itself crash and never reach the "处理异常" reply.
func TestNotifyCtx_NilParentSafe(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NotifyCtx panicked on nil parent: %v", r)
		}
	}()

	notify, cancel := NotifyCtx(nil, NotifyKindAskQuestionCard, 100*time.Millisecond)
	defer cancel()
	if notify == nil {
		t.Fatal("NotifyCtx returned nil ctx for nil parent")
	}
	if notify.Err() != nil {
		t.Fatalf("NotifyCtx returned already-cancelled ctx for nil parent: %v", notify.Err())
	}
}
