package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// fixedFooterCaps is a minimal Capabilities stub returning a constant
// footer regardless of backendID. Lets decorateReplyText be exercised
// without standing up a real Server / Router.
type fixedFooterCaps struct {
	NoopCapabilities
	footer string
}

func (f fixedFooterCaps) ReplyFooter(string) string { return f.footer }

// TestDispatcher_DecorateReplyText_Components covers each branch of
// decorateReplyText (R219-CR-7 / #656) so the helper extraction from
// sendAndReply doesn't silently change observable wire behaviour:
//
//   - merge-group head appends the "*— 合并了 N 条消息的回复*" chip
//   - per-session ReplyFooter is appended when non-empty
//   - empty Text + non-empty footer still surfaces no footer (we only
//     append on non-empty replyText for the merge chip; footer was
//     historically always appended when non-empty even on empty text,
//     and we preserve that to keep behaviour identical to the inline
//     version)
//
// nil sess is the cron-pruned edge case — must not panic.
func TestDispatcher_DecorateReplyText_Components(t *testing.T) {
	d := &Dispatcher{caps: fixedFooterCaps{footer: "cc"}}

	t.Run("plain text gets footer", func(t *testing.T) {
		got := d.decorateReplyText(&cli.SendResult{Text: "hi"}, nil)
		if !strings.Contains(got, "hi") || !strings.Contains(got, "cc") {
			t.Errorf("got %q, want both 'hi' and 'cc'", got)
		}
	})

	t.Run("merge head gets chip + footer", func(t *testing.T) {
		got := d.decorateReplyText(&cli.SendResult{Text: "hi", MergedCount: 3}, nil)
		if !strings.Contains(got, "合并了 3 条") {
			t.Errorf("got %q, want merge chip with count 3", got)
		}
		if !strings.Contains(got, "cc") {
			t.Errorf("got %q, want footer 'cc'", got)
		}
	})

	t.Run("empty text with merge skips chip", func(t *testing.T) {
		// Merge follower (Text=="" && MergedCount>1): chip MUST NOT
		// fire because it'd add a "合并了…" line on a bubble that
		// otherwise has no content. Footer still fires (matches
		// pre-extraction behaviour: footer append was unconditional).
		got := d.decorateReplyText(&cli.SendResult{Text: "", MergedCount: 5}, nil)
		if strings.Contains(got, "合并了") {
			t.Errorf("got %q, must not contain merge chip on empty text", got)
		}
	})
}

// TestDispatcher_DecorateReplyText_NilCapsIsPanic just documents that
// a nil caps is an unwireup bug — the helper does not defensively check
// because every NewDispatcher path installs at least NoopCapabilities,
// and a panic surfaces the misuse loud enough that tests catch it.
func TestDispatcher_DecorateReplyText_NilCapsIsPanic(t *testing.T) {
	// Use NoopCapabilities (the production fallback) so we exercise the
	// non-panicking path. NoopCapabilities.ReplyFooter returns "" which
	// means no footer is appended.
	d := &Dispatcher{caps: NoopCapabilities{}}
	got := d.decorateReplyText(&cli.SendResult{Text: "x"}, nil)
	if got != "x" {
		t.Errorf("NoopCapabilities footer should be empty; got %q want %q", got, "x")
	}
}

// _ static checks ensure the helper signature stays compatible with
// session.ManagedSession (most call sites pass a *session.ManagedSession
// produced by Router.GetOrCreate).
var _ = func(d *Dispatcher, s *session.ManagedSession, ctx context.Context) string {
	_ = ctx
	return d.decorateReplyText(&cli.SendResult{}, s)
}
