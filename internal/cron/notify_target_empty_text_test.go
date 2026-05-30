package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestNotifyTargetEmptyTextNoSend pins the #1116 contract at the
// notifyTarget layer: an empty-text notice must not reach platform.Reply
// at all. deliverNotice already short-circuits its async path, but
// notifyTarget is reachable directly (in-package + future call sites),
// where platform.SplitText("", maxLen) returns [""] and the chunk loop
// would otherwise burn one limits.PlatformReplyMaxAttempts retry budget
// pushing a zero-byte chunk.
func TestNotifyTargetEmptyTextNoSend(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	}

	s.notifyTarget("fake-notify", "chat-x", "")

	if got := fp.sendCount.Load(); got != 0 {
		t.Errorf("Reply called %d times for empty-text notice, want 0 (#1116 short-circuit must fire before SplitText/chunk loop)", got)
	}
	if got := fp.uniqueChunks(); got != 0 {
		t.Errorf("unique chunks attempted = %d for empty text, want 0", got)
	}
}

// TestNotifyTargetNonEmptyStillSends guards against the empty-text
// short-circuit accidentally swallowing legitimate single-chunk notices.
func TestNotifyTargetNonEmptyStillSends(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	}

	s.notifyTarget("fake-notify", "chat-x", "hi")

	if got := fp.sendCount.Load(); got == 0 {
		t.Error("Reply not called for non-empty notice; the empty-text guard is over-eager")
	}
}
