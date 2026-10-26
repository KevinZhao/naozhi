package dispatch

import (
	"context"
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
