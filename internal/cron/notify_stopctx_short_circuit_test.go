package cron

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestR20260527122801GO014_NotifyTargetShortCircuitsOnCancelledStopCtx
// pins the cheap pre-SplitText guard added in scheduler_notify.go's
// notifyTarget. When deliverNotice's goroutine is scheduled after Stop
// has already cancelled stopCtx, the existing replyCtx.Err() check at
// the chunk-loop boundary (line ~188) eventually catches it — but only
// after platform.SplitText has walked the whole result string and
// allocated a chunks slice. Long results turn that into a non-trivial
// alloc that's pure waste on the shutdown path.
//
// Pre-fix: notifyTarget invokes p.MaxReplyLength + SplitText + chunk
// loop, then bails on the first replyCtx.Err() check. Post-fix: bail
// at the function head before touching s.platforms[plat] or any text
// processing. The platform's Reply MUST NOT fire when stopCtx is
// already cancelled at entry.
func TestR20260527122801GO014_NotifyTargetShortCircuitsOnCancelledStopCtx(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	stopCancel() // already dead before notifyTarget is called
	s := &Scheduler{
		platforms: map[string]platform.Platform{"fake-notify": fp},
		stopCtx:   stopCtx,
	}
	// Long enough to make SplitText's chunk walk visibly nontrivial —
	// not asserted directly (we lack a hook into the alloc counter),
	// but the Reply-not-called assertion proves the short-circuit ran
	// before any chunk processing.
	long := buildDistinctChunks(50, 8)
	s.notifyTarget("fake-notify", "chat-x", long)
	if got := fp.uniqueChunks(); got != 0 {
		t.Errorf("Reply invoked %d times after stopCtx was already cancelled; want 0 (short-circuit must run before chunk loop)", got)
	}
	if got := fp.sendCount.Load(); got != 0 {
		t.Errorf("Reply sendCount = %d after stopCtx cancel; want 0", got)
	}
}

// TestR20260527122801GO014_NotifyTargetNilStopCtxDoesNotShortCircuit
// guards the defensive nil-stopCtx fallback path. A hand-constructed
// *Scheduler (e.g. test fake) without stopCtx wired must keep working
// — the new short-circuit only triggers when stopCtx is non-nil AND
// already errored.
func TestR20260527122801GO014_NotifyTargetNilStopCtxDoesNotShortCircuit(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{
		platforms: map[string]platform.Platform{"fake-notify": fp},
		// stopCtx intentionally nil
	}
	s.notifyTarget("fake-notify", "chat-x", "hello world")
	if fp.uniqueChunks() == 0 {
		t.Errorf("notifyTarget with nil stopCtx delivered 0 chunks; short-circuit guard fired on a nil parent")
	}
}
