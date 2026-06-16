package dispatch

// #2147: on single-use-token platforms (Weixin iLink) a standalone TodoWrite
// Reply must NOT be sent. The context_token is consumed by the first Reply and
// rejected on reuse, so a TodoWrite checklist would burn the token before the
// final answer is delivered via sendAndReply, causing the real answer to be
// rejected upstream and silently lost.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/platform"
)

// fakeSingleUsePlatform implements platform.Platform with
// UsesSingleUseReplyToken() == true and SupportsInterimMessages() == false
// (Weixin-like).
type fakeSingleUsePlatform struct {
	mu      sync.Mutex
	replies []platform.OutgoingMessage
}

func (f *fakeSingleUsePlatform) Name() string                                               { return "fake-singleuse" }
func (f *fakeSingleUsePlatform) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (f *fakeSingleUsePlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, msg)
	return "msg-1", nil
}
func (f *fakeSingleUsePlatform) EditMessage(_ context.Context, _ string, _ string) error { return nil }
func (f *fakeSingleUsePlatform) MaxReplyLength() int                                     { return 4000 }
func (f *fakeSingleUsePlatform) SupportsInterimMessages() bool                           { return false }
func (f *fakeSingleUsePlatform) UsesSingleUseReplyToken() bool                           { return true }
func (f *fakeSingleUsePlatform) replyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies)
}

func todoWriteEvent(t *testing.T) cli.Event {
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
	return cli.Event{
		Type:    "assistant",
		Message: &cli.AssistantMessage{Content: []cli.ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: input}}},
	}
}

// TestReplyTracker_TodoWrite_SuppressedOnSingleUseToken verifies the #2147 fix:
// a TodoWrite event on a single-use-token platform fires zero Reply calls, so
// the lone context_token survives for the final answer.
func TestReplyTracker_TodoWrite_SuppressedOnSingleUseToken(t *testing.T) {
	t.Parallel()

	fp := &fakeSingleUsePlatform{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct")
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

	tracker := newIMEventTracker(ctx, fp, "chat1", "direct")
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
