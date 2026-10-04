package dispatch

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestDecorateReplyText_TurnFailure: a failed turn's reply is a notice with
// the usual footer, never silence or the backend's raw RPC text, while an
// abort naozhi asked for stays silent.
func TestDecorateReplyText_TurnFailure(t *testing.T) {
	d := &Dispatcher{caps: fixedFooterCaps{footer: "cc"}}
	tests := []struct {
		name    string
		r       *clievent.SendResult
		want    []string // "" entries skipped; nil = want the empty sentinel
		notWant []string
	}{
		{
			name: "empty max turns gets the notice and footer",
			r:    &clievent.SendResult{SubType: "error_max_turns", IsError: true},
			want: []string{"⚠️ ", "最大执行步数", "— cc"},
		},
		{
			name: "empty during-execution failure gets the notice",
			r:    &clievent.SendResult{SubType: "error_during_execution", IsError: true},
			want: []string{"⚠️ ", "中途出错"},
		},
		{
			name: "own abort stays silent",
			r:    &clievent.SendResult{SubType: "error_during_execution", IsError: true, Aborted: true},
		},
		{
			name: "rpc overload hides the raw text",
			r: &clievent.SendResult{
				Text: "[codex] codex rpc error -32001: Server overloaded", SubType: "error", IsError: true,
				BackendError: &clievent.BackendError{Backend: "codex", Code: -32001, Message: "Server overloaded"},
			},
			want:    []string{"⚠️ codex 服务当前负载较高", "— cc"},
			notWant: []string{"-32001", "rpc error", "Server overloaded"},
		},
		{
			name: "is_error text without prefix is localized",
			r:    &clievent.SendResult{Text: "Prompt is too long", SubType: "success", IsError: true},
			want: []string{"📏 对话上下文已超出模型上限"},
		},
		{
			name: "unrecognised is_error text passes through",
			r:    &clievent.SendResult{Text: "Execution error", IsError: true},
			want: []string{"Execution error", "— cc"},
		},
		{
			name: "is_error text is redacted",
			r:    &clievent.SendResult{Text: "bad key sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx", IsError: true},
			want: []string{"[REDACTED]"}, notWant: []string{"sk-ant-api03"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.decorateReplyText(tt.r, nil)
			if tt.want == nil {
				if got != "" {
					t.Fatalf("got %q, want the empty sentinel", got)
				}
				return
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("got %q, missing %q", got, w)
				}
			}
			for _, bad := range tt.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("got %q, must not contain %q", got, bad)
				}
			}
		})
	}
}

// TestDecorateReplyText_CountsTurnFailureByClass: each failed result bumps
// its class in naozhi_dispatch_turn_error_result_total; an answer does not.
func TestDecorateReplyText_CountsTurnFailureByClass(t *testing.T) {
	d := &Dispatcher{caps: NoopCapabilities{}}
	count := func(class string) int64 {
		if v := dispatchTurnErrorResultTotal.Get(class); v != nil {
			return v.(interface{ Value() int64 }).Value()
		}
		return 0
	}
	before, beforeText := count("max_budget"), count("error_text")
	d.decorateReplyText(&clievent.SendResult{SubType: "error_max_budget_usd", IsError: true}, nil)
	d.decorateReplyText(&clievent.SendResult{Text: "Execution error", IsError: true}, nil)
	d.decorateReplyText(&clievent.SendResult{Text: "fine", SubType: "success"}, nil)
	if got := count("max_budget") - before; got != 1 {
		t.Errorf("max_budget count moved by %d, want 1", got)
	}
	if got := count("error_text") - beforeText; got != 1 {
		t.Errorf("error_text count moved by %d, want 1", got)
	}
}

// deliverWithBanner runs imDelivery.reply on a turn that posted a progress
// banner and returns the banner edits and the replies after the banner.
func deliverWithBanner(t *testing.T, r *clievent.SendResult) (edits, replies []string) {
	t.Helper()
	p := &discordLikePlatform{fakePlatform: fakePlatform{supportsInterim: true, replyMsgID: "banner-1"}}
	d := newTestDispatcher(&fakePlatform{})
	d.platforms = map[string]platform.Platform{"fake": p}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := incomingMsg("hello")
	o := d.newIMOrigin(msg, slog.Default(), "fake:direct:chat1:general", "general", session.AgentOpts{}, imMessage, len(msg.Text), 0)
	dl := &imDelivery{o: o, p: p, lg: slog.Default()}
	dl.BeforeSession(ctx)
	onEvent := dl.SessionReady(ctx, sessionview.SessionExisting)
	onEvent(clievent.Event{
		Type:    "assistant",
		Message: &clievent.AssistantMessage{Content: []clievent.ContentBlock{{Type: "tool_use", Name: "Bash"}}},
	})
	dl.tracker.waitReady(ctx)
	if dl.tracker.getThinkingMsgID() == "" {
		t.Fatal("no progress banner was posted")
	}
	dl.reply(ctx, r, nil)
	dl.tracker.stop()
	edits, _ = p.edited()
	return edits, p.allReplies()[1:]
}

// TestIMDeliveryReply_FailedTurnReplacesBanner: a failed empty turn's notice
// is edited into the progress banner instead of leaving its last tool status.
func TestIMDeliveryReply_FailedTurnReplacesBanner(t *testing.T) {
	t.Parallel()
	edits, replies := deliverWithBanner(t, &clievent.SendResult{SubType: "error_max_turns", IsError: true})
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "最大执行步数") {
		t.Errorf("banner edits = %q, want the last one to be the max-turns notice", edits)
	}
	if len(replies) != 0 {
		t.Errorf("sent %q besides the banner, want nothing", replies)
	}
}

// TestIMDeliveryReply_AbortedTurnMarksBanner: an aborted turn sends nothing
// new; its banner reads bannerAborted.
func TestIMDeliveryReply_AbortedTurnMarksBanner(t *testing.T) {
	t.Parallel()
	edits, replies := deliverWithBanner(t, &clievent.SendResult{SubType: "error_during_execution", IsError: true, Aborted: true})
	if len(edits) == 0 || edits[len(edits)-1] != bannerAborted {
		t.Errorf("banner edits = %q, want the last one %q", edits, bannerAborted)
	}
	if len(replies) != 0 {
		t.Errorf("sent %q besides the banner, want nothing", replies)
	}
}

// TestIMDeliveryReply_FailedMergeHeadGetsNotice: the head of a merged turn
// (MergedWithHead 0) that failed or was aborted with no text is not a merge
// follower; it gets the notice or the aborted banner, never the merge hint.
func TestIMDeliveryReply_FailedMergeHeadGetsNotice(t *testing.T) {
	t.Parallel()
	edits, replies := deliverWithBanner(t, &clievent.SendResult{SubType: "error_max_turns", IsError: true, MergedCount: 2})
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "最大执行步数") {
		t.Errorf("failed head: banner edits = %q, want the last one to be the max-turns notice", edits)
	}
	if len(replies) != 0 {
		t.Errorf("failed head: sent %q besides the banner, want nothing", replies)
	}

	edits, replies = deliverWithBanner(t, &clievent.SendResult{SubType: "error_during_execution", IsError: true, Aborted: true, MergedCount: 3})
	if len(edits) == 0 || edits[len(edits)-1] != bannerAborted {
		t.Errorf("aborted head: banner edits = %q, want the last one %q", edits, bannerAborted)
	}
	if len(replies) != 0 {
		t.Errorf("aborted head: sent %q besides the banner, want nothing", replies)
	}
}
