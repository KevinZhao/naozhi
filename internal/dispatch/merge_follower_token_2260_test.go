package dispatch

// #2260: on a single-use reply-token platform (WeChat/iLink) the merge-follower
// ack must NOT fall back to a text reply — the shared context_token was already
// consumed by the head slot, so a follower Reply is rejected upstream (notice
// silently lost) and worse races the head's real answer for the cached token.

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// fakeSingleUseReactorless is a single-use-token platform that is NOT a
// Reactor (Weixin-like): no AddReaction surface, so ackMergedFollower's
// reaction arm returns false and it would otherwise fall through to replyText.
type fakeSingleUseReactorless struct {
	mu      sync.Mutex
	replies []platform.OutgoingMessage
}

func (f *fakeSingleUseReactorless) Name() string                                               { return "fake-su" }
func (f *fakeSingleUseReactorless) RegisterRoutes(_ *http.ServeMux, _ platform.MessageHandler) {}
func (f *fakeSingleUseReactorless) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, msg)
	return "msg-1", nil
}
func (f *fakeSingleUseReactorless) EditMessage(_ context.Context, _, _ string) error { return nil }
func (f *fakeSingleUseReactorless) MaxReplyLength() int                              { return 4000 }
func (f *fakeSingleUseReactorless) SupportsInterimMessages() bool                    { return false }
func (f *fakeSingleUseReactorless) UsesSingleUseReplyToken() bool                    { return true }
func (f *fakeSingleUseReactorless) replyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replies)
}

var _ platform.Platform = (*fakeSingleUseReactorless)(nil)

// TestAckMergedFollower_SingleUseToken_SkipsTextFallback verifies the #2260
// fix: on a single-use-token platform that is not a Reactor, ackMergedFollower
// must NOT send the "已合并到上一条回复。" text fallback (it would reuse the
// spent context_token, get rejected upstream, and race the head's real answer).
func TestAckMergedFollower_SingleUseToken_SkipsTextFallback(t *testing.T) {
	t.Parallel()

	fp := &fakeSingleUseReactorless{}
	d := newTestDispatcher(&fakePlatform{}, nil)
	d.platforms = map[string]platform.Platform{fp.Name(): fp}

	msg := platform.IncomingMessage{Platform: fp.Name(), MessageID: "m1", ChatID: "c1"}
	d.ackMergedFollower(context.Background(), msg, "fake:direct:c1", 3, nil)

	if n := fp.replyCount(); n != 0 {
		t.Errorf("#2260: single-use-token follower fired %d Reply calls (token reused); want 0", n)
	}
}

// TestAckMergedFollower_MultiSend_StillSendsTextFallback pins the positive
// case: a non-reactor, multi-send platform still gets the merge text notice.
func TestAckMergedFollower_MultiSend_StillSendsTextFallback(t *testing.T) {
	t.Parallel()

	fp := &fakePlatform{} // multi-send, not a Reactor
	d := newTestDispatcher(fp, nil)

	msg := platform.IncomingMessage{Platform: "fake", MessageID: "m1", ChatID: "c1"}
	d.ackMergedFollower(context.Background(), msg, "fake:direct:c1", 2, nil)

	if got := fp.lastReply(); got != "已合并到上一条回复。" {
		t.Errorf("#2260: multi-send follower text fallback = %q; want the merge notice", got)
	}
}
