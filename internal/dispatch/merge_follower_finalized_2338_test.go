package dispatch

// #2338: the passthrough merge-follower early-return path edits the banner to
// the merge hint but historically never called markFinalized(). Because the
// tracker's editLoop survives across the deferred stop(), a residual buffered
// editCh signal (left by the interim fan-out that posted the "💭思考中…" banner)
// could wake editLoop AFTER the merge-hint edit and repaint the stale status,
// orphaning the user on a "thinking…" bubble that never resolves. The fix
// applies the same markFinalized() guard the success path uses (#2291) before
// the follower's EditMessage.

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestMergeFollower_FinalizedBlocksResidualRepaint replays the follower
// early-return collapse logic (waitReady → markFinalized → EditMessage) exactly
// as the dispatch.go branch does, then injects a residual editCh signal and
// asserts the live editLoop does NOT repaint the stale "💭思考中…" status over
// the merge hint.
func TestMergeFollower_FinalizedBlocksResidualRepaint(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{supportsInterim: true, replyMsgID: "banner-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct", "general")

	// Interim assistant event posts the "💭思考中…" banner on this follower slot
	// and leaves a buffered editCh signal (process_readloop interim fan-out).
	tracker.onEvent(cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "text", Text: "working"}}},
	})

	// Follower carries no final text. Replay the fixed early-return branch:
	// waitReady, markFinalized (the #2338 fix), then collapse to the merge hint.
	tracker.waitReady(ctx)
	tracker.markFinalized()
	msgID := tracker.getThinkingMsgID()
	if msgID == "" {
		t.Fatal("#2338 setup: interim event did not post a banner (no thinkingMsgID)")
	}
	if err := fp.EditMessage(ctx, msgID, "已合并到上一条回复。"); err != nil {
		t.Fatalf("edit banner: %v", err)
	}

	// A residual buffered editCh signal fires after the merge-hint edit but
	// before the deferred stop() — exactly the #2338 race window. With the
	// finalize guard in place editLoop must drop it.
	select {
	case tracker.editCh <- struct{}{}:
	default:
	}

	// Give editLoop time to wake on the residual signal and (correctly) skip.
	time.Sleep(120 * time.Millisecond)
	tracker.stop()

	fp.mu.Lock()
	edits := append([]fakeEdit(nil), fp.edits...)
	fp.mu.Unlock()

	if len(edits) == 0 {
		t.Fatal("#2338: expected at least the merge-hint edit; got none")
	}
	// The final edit must be the merge hint, not a stale "思考中" repaint.
	last := edits[len(edits)-1]
	if last.text != "已合并到上一条回复。" {
		t.Errorf("#2338: final banner edit = %q, want merge hint (residual editCh repainted stale status); edits=%+v",
			last.text, edits)
	}
	for _, e := range edits {
		if e.text != "已合并到上一条回复。" {
			t.Errorf("#2338: stray non-merge-hint edit %q after finalize; edits=%+v", e.text, edits)
		}
	}
}
