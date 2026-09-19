package dispatch

// #2291: once sendAndReply has committed (or is about to commit) the final
// answer to the banner, a residual buffered editCh signal must NOT trigger a
// status redraw that overwrites the real answer with stale interim status.

import (
	"context"
	"testing"
	"time"
)

// TestEditLoop_SkipsRedrawAfterFinalized verifies the #2291 fix: after
// markFinalized, editLoop drops a pending editCh signal instead of repainting.
func TestEditLoop_SkipsRedrawAfterFinalized(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{supportsInterim: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct", "")

	// Simulate a banner having been posted and some interim status queued.
	id := "banner-1"
	tracker.thinkingMsgID.Store(&id)
	tracker.linesMu.Lock()
	tracker.statusLines = appendStatusLine(tracker.statusLines, "💭 思考中…")
	tracker.linesMu.Unlock()

	// Final answer is being committed: mark finalized, then leave a residual
	// buffered editCh signal as the readloop would after a late interim event.
	tracker.markFinalized()
	select {
	case tracker.editCh <- struct{}{}:
	default:
	}

	// Release editLoop (it parks on msgIDReady until waitReady closes it).
	tracker.waitReady(ctx)

	// Give editLoop time to wake on the residual signal and (correctly) skip.
	time.Sleep(120 * time.Millisecond)

	fp.mu.Lock()
	n := len(fp.edits)
	got := append([]fakeEdit(nil), fp.edits...)
	fp.mu.Unlock()
	if n != 0 {
		t.Errorf("#2291: editLoop performed %d edit(s) after finalize, want 0: %+v", n, got)
	}

	tracker.stop()
}
