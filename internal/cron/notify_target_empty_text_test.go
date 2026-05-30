package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestR250PERF13_NotifyTargetEmptyTextNoSend pins the #1116 contract at the
// send layer: notifyTarget("", ...) must be a no-op and must NOT invoke
// platform.Reply. platform.SplitText("", maxLen) returns [""] — one empty
// chunk — so without the guard the chunk loop would burn a full
// limits.PlatformReplyMaxAttempts retry budget pushing a zero-byte message.
// deliverNotice already short-circuits empty notices, but notifyTarget is
// reachable directly and owns the chunk loop, so the no-op contract lives
// here too.
func TestR250PERF13_NotifyTargetEmptyTextNoSend(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	}

	s.notifyTarget("fake-notify", "chat-x", "")

	if n := fp.sendCount.Load(); n != 0 {
		t.Errorf("Reply invoked %d times for empty text; want 0 (notifyTarget must short-circuit before the chunk loop)", n)
	}
}
