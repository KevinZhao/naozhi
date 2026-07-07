package dispatch

// #2290: when a merge-follower's tracker already posted a "💭思考中…" banner
// (an interim assistant event fanned out to its onEvent before the merge
// collapsed the turn), the follower early-return must collapse that banner to
// the merge hint rather than leave an orphaned thinking bubble.

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestMergeFollower_CollapsesInterimBanner verifies the #2290 fix at the
// tracker level, exercising the exact primitives the fix uses (waitReady →
// getThinkingMsgID → EditMessage), which run before the CLI-dependent
// caps.Send so they are isolable from router spawning.
func TestMergeFollower_CollapsesInterimBanner(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{supportsInterim: true, replyMsgID: "banner-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct", "general")

	// Interim assistant event posts the "💭思考中…" banner on this follower
	// slot (process_readloop interim fan-out claims all currentTurnSlots).
	tracker.onEvent(cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "text", Text: "working"}}},
	})

	// The merge then collapses the turn: follower carries no final text.
	// Replay the fix's follower-branch collapse logic.
	tracker.waitReady(ctx)
	if msgID := tracker.getThinkingMsgID(); msgID != "" {
		if err := fp.EditMessage(ctx, msgID, "已合并到上一条回复。"); err != nil {
			t.Fatalf("edit banner: %v", err)
		}
	} else {
		t.Fatal("#2290 setup: interim event did not post a banner (no thinkingMsgID)")
	}
	tracker.stop()

	fp.mu.Lock()
	edits := append([]fakeEdit(nil), fp.edits...)
	fp.mu.Unlock()

	var collapsed bool
	for _, e := range edits {
		if e.msgID == "banner-1" && e.text == "已合并到上一条回复。" {
			collapsed = true
		}
	}
	if !collapsed {
		t.Errorf("#2290: follower banner not collapsed to merge hint; edits=%+v", edits)
	}
}
