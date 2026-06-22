package cron

// notify_target_single_use_test.go pins the #2181 contract (cron companion to
// dispatch's #2136): when notifyTarget delivers a long result to a platform
// whose reply is authorised by a single-use token (e.g. WeChat iLink), it must
// collapse to exactly ONE rune-safe-truncated message rather than fanning into
// N chunks. The token is consumed by the first send; chunks 2..N would be
// silently lost upstream. The single-use bit flows through the
// cron.PlatformReplier interface (the wireup adapter / test fake delegate to
// platform.UsesSingleUseReplyToken) so cron never imports internal/platform.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/platform"
)

// fakeSingleUseNotifyPlatform models a WeChat-iLink-style platform: the first
// Reply succeeds (consuming the context_token) and every subsequent Reply
// fails — exactly the loss mode #2181 guards against. UsesSingleUseReplyToken
// returns true so platform.UsesSingleUseReplyToken (and thus the cron
// PlatformReplier adapter) reports the single-use bit.
type fakeSingleUseNotifyPlatform struct {
	maxLen int

	mu      sync.Mutex
	replies []string // Text of each Reply attempt, in order
}

func (f *fakeSingleUseNotifyPlatform) Name() string                                           { return "fake-singleuse" }
func (f *fakeSingleUseNotifyPlatform) RegisterRoutes(*http.ServeMux, platform.MessageHandler) {}
func (f *fakeSingleUseNotifyPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, msg.Text)
	if len(f.replies) > 1 {
		// Token already consumed by the first send — reuse rejected upstream.
		return "", errors.New("context_token already used")
	}
	return "msg-1", nil
}
func (f *fakeSingleUseNotifyPlatform) EditMessage(context.Context, string, string) error { return nil }
func (f *fakeSingleUseNotifyPlatform) MaxReplyLength() int                               { return f.maxLen }
func (f *fakeSingleUseNotifyPlatform) UsesSingleUseReplyToken() bool                     { return true }
func (f *fakeSingleUseNotifyPlatform) sentReplies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.replies))
	copy(out, f.replies)
	return out
}

// TestR2181_NotifyTargetSingleUseCollapsesToOneTruncatedMessage verifies that a
// long result destined for a single-use-token platform is delivered as exactly
// one message, rune-safe-truncated with the visible marker when it exceeds
// maxLen, and that no second Reply is attempted (which would fail and burn the
// already-consumed token).
func TestR2181_NotifyTargetSingleUseCollapsesToOneTruncatedMessage(t *testing.T) {
	t.Parallel()
	const maxLen = 20
	fp := &fakeSingleUseNotifyPlatform{maxLen: maxLen}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-singleuse": fp})

	// A result well over maxLen that would otherwise fan into many chunks.
	long := strings.Repeat("一二三四五", 30) // 150 runes, all multi-byte
	if utf8.RuneCountInString(long) <= maxLen {
		t.Fatalf("fixture too short: %d runes <= maxLen %d", utf8.RuneCountInString(long), maxLen)
	}

	s.notifyTarget("fake-singleuse", "chat-x", long)

	replies := fp.sentReplies()
	if len(replies) != 1 {
		t.Fatalf("#2181: single-use platform got %d Reply attempts, want exactly 1 (token is single-use)", len(replies))
	}
	got := replies[0]
	// Rune-safe: must be valid UTF-8 and within the maxLen rune budget.
	if !utf8.ValidString(got) {
		t.Errorf("#2181: collapsed reply is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > maxLen {
		t.Errorf("#2181: collapsed reply has %d runes, exceeds maxLen %d", n, maxLen)
	}
	if !strings.HasSuffix(got, singleReplyTruncMarker) {
		t.Errorf("#2181: over-length collapsed reply must carry the truncation marker %q; got %q", singleReplyTruncMarker, got)
	}
}

// TestR2181_NotifyTargetSingleUseShortResultNotTruncated pins that the collapse
// only truncates when the rune length exceeds maxLen: a short result is sent
// verbatim as one message with no marker.
func TestR2181_NotifyTargetSingleUseShortResultNotTruncated(t *testing.T) {
	t.Parallel()
	const maxLen = 50
	fp := &fakeSingleUseNotifyPlatform{maxLen: maxLen}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-singleuse": fp})

	short := "短结果，无需截断"
	s.notifyTarget("fake-singleuse", "chat-x", short)

	replies := fp.sentReplies()
	if len(replies) != 1 {
		t.Fatalf("#2181: single-use platform got %d Reply attempts, want exactly 1", len(replies))
	}
	if replies[0] != short {
		t.Errorf("#2181: short single-use reply must be sent verbatim; got %q want %q", replies[0], short)
	}
}

// TestR2181_NotifyTargetMultiSendStillFansIntoChunks is the negative case: a
// platform that does NOT use a single-use token (UsesSingleUseReplyToken==false
// via the default platform capability) must still fan a long result into the
// normal N chunks — the collapse path is single-use-only.
func TestR2181_NotifyTargetMultiSendStillFansIntoChunks(t *testing.T) {
	t.Parallel()
	// fakePartialPlatform (notify_target_partial_test.go) does NOT implement
	// SingleUseReplyTokenCapable, so platform.UsesSingleUseReplyToken returns
	// the default false. failAt high so every chunk succeeds.
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})

	// 3 chunks (< cap=5 so the chunk cap doesn't interfere).
	multi := buildDistinctChunks(3, 8)
	wantChunks := len(platform.SplitText(multi, 8))
	if wantChunks < 2 {
		t.Fatalf("fixture must produce >=2 chunks to prove fan-out; got %d", wantChunks)
	}

	s.notifyTarget("fake-notify", "chat-x", multi)

	if got := fp.uniqueChunks(); got != wantChunks {
		t.Errorf("#2181 negative: multi-send platform should fan into %d chunks, got %d (collapse must be single-use-only)", wantChunks, got)
	}
}
