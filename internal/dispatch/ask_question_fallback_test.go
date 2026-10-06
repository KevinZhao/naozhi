package dispatch

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestSendAskQuestionFallback_Text pins the plain-text option list a platform
// without native question cards receives, byte for byte.
func TestSendAskQuestionFallback_Text(t *testing.T) {
	t.Parallel()
	fp := &fakeInterimPlatform{}
	tr := &replyTracker{p: fp, to: ReplyDest{ChatID: "chat1"}}
	tr.sendAskQuestionFallback(context.Background(), &clievent.AskQuestion{
		ToolUseID: "t1",
		Items: []clievent.AskQuestionItem{
			{Question: "Which approach?", Header: "Error style", Options: []clievent.AskQuestionOpt{
				{Label: "Return an error", Description: "idiomatic Go"},
				{Label: "Panic"},
			}},
			{Question: "Target?", MultiSelect: true, Options: []clievent.AskQuestionOpt{{Label: "A"}}},
		},
	})
	const want = "Claude 想请你确认：\n" +
		"\n【Error style】Which approach?\n" +
		"  1. Return an error — idiomatic Go\n" +
		"  2. Panic\n" +
		"\n问题 2：Target?\n" +
		"  1. A\n" +
		"\n直接回复选项内容即可（例如：「Error style: Return an error」）。"
	if len(fp.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(fp.replies))
	}
	if got := fp.replies[0]; got.ChatID != "chat1" || got.Text != want {
		t.Errorf("fallback reply = %+v\nwant text %q", got, want)
	}
}
