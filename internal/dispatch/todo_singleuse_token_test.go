package dispatch

// #2147: on single-use-token platforms (Weixin iLink) a standalone TodoWrite
// Reply must NOT be sent. The context_token is consumed by the first Reply and
// rejected on reuse, so a TodoWrite checklist would burn the token before the
// final answer is delivered via the IM delivery, causing the real answer to be
// rejected upstream and silently lost.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func todoWriteEvent(t *testing.T) clievent.Event {
	t.Helper()
	input, err := json.Marshal(map[string]any{
		"todos": []map[string]any{
			{"content": "step one", "status": "pending"},
			{"content": "step two", "status": "in_progress", "activeForm": "doing step two"},
		},
	})
	if err != nil {
		t.Fatalf("marshal todos: %v", err)
	}
	return clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: input}}},
	}
}

// TestReplyTracker_TodoWrite_SuppressedOnSingleUseToken verifies the #2147 fix:
// a TodoWrite event on a single-use-token platform fires zero Reply calls, so
// the lone context_token survives for the final answer.
func TestReplyTracker_TodoWrite_SuppressedOnSingleUseToken(t *testing.T) {
	t.Parallel()

	fp := &fakeSingleUseReactorless{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, ReplyDest{ChatID: "chat1"}, "direct", "")
	defer tracker.stop()

	tracker.onEvent(todoWriteEvent(t))

	// Give any async todoLoop Reply goroutine time to run if the fix is absent.
	time.Sleep(80 * time.Millisecond)

	if n := fp.replyCount(); n != 0 {
		t.Errorf("#2147: TodoWrite on single-use-token platform fired %d Reply calls (token burned); want 0", n)
	}
}

// TestReplyTracker_TodoWrite_DeliveredOnMultiSend pins the positive case: a
// platform that allows multiple sends (Feishu-like) still surfaces the
// TodoWrite checklist as a standalone Reply.
func TestReplyTracker_TodoWrite_DeliveredOnMultiSend(t *testing.T) {
	t.Parallel()

	fp := &fakeInterimPlatform{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, ReplyDest{ChatID: "chat1"}, "direct", "")
	defer tracker.stop()

	tracker.onEvent(todoWriteEvent(t))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fp.replyCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if fp.replyCount() == 0 {
		t.Error("#2147: TodoWrite on multi-send platform must still surface checklist Reply")
	}
}
