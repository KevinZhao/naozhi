package dispatch

// #2262: ownerLoop's happy-path reaction clear must detach from the (possibly
// already-cancelled) turn ctx via context.WithoutCancel — otherwise a
// shutdown-during-turn race makes the child WithTimeout born cancelled, every
// RemoveReaction short-circuits, and the ⏳ HOURGLASS hangs until the
// platform's ~12h reaction TTL.

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestClearQueuedReactions_DetachedCtxSurvivesCancel verifies the #2262 fix
// premise: clearing reactions through context.WithoutCancel of an
// already-cancelled parent still issues RemoveReaction.
func TestClearQueuedReactions_DetachedCtxSurvivesCancel(t *testing.T) {
	t.Parallel()

	fp := &fakeReactorPlatform{}
	d := newTestDispatcherWithPlatform(fp)
	d.platforms = map[string]platform.Platform{fp.Name(): fp}

	parent, cancel := context.WithCancel(context.Background())
	cancel() // ownerLoop's stopCtx already Done (shutdown-during-turn)

	queued := []QueuedMsg{{MessageID: "m1"}, {MessageID: "m2"}}
	d.clearQueuedReactions(context.WithoutCancel(parent), fp.Name(), queued, nil)

	fp.mu.Lock()
	got := len(fp.removed)
	fp.mu.Unlock()
	if got != 2 {
		t.Errorf("#2262: detached clear issued %d RemoveReaction calls, want 2", got)
	}
}
