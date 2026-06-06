package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestR236SEC15_NotifyTargetCapsChunkCount pins the #568 contract:
// when SplitText would yield more than cronNotifyMaxChunks chunks,
// notifyTarget delivers only the cap and slog-Warns the dropped tail
// rather than running the chunks × retries × per-attempt loop until
// cronNotifyTimeout fires mid-flush.
//
// Pre-fix behaviour: composite worst-case (chunks × PlatformReplyMaxAttempts
// × platformReplyTimeout) could exceed the 30s cronNotifyTimeout on a slow
// platform, leaving the user with a partially-truncated message and no
// observable cap on alloc / slog volume.
//
// Post-fix: chunks beyond the cap are dropped before the loop with a
// single aggregated WARN; the surviving chunks are sent in order.
func TestR236SEC15_NotifyTargetCapsChunkCount(t *testing.T) {
	t.Parallel()
	// failAt larger than the cap so every uncapped chunk that reaches
	// Reply succeeds — we want to assert the cap, not a partial-failure
	// abort. maxLen=8 keeps SplitText boundaries deterministic with
	// buildDistinctChunks.
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})
	// Build 10 distinct chunks; cap is 5.
	long := buildDistinctChunks(10, 8)
	totalChunks := len(platform.SplitText(long, 8))
	if totalChunks <= cronNotifyMaxChunks {
		t.Fatalf("test fixture insufficient: SplitText produced %d chunks but cap is %d (need > cap to exercise truncation)", totalChunks, cronNotifyMaxChunks)
	}

	s.notifyTarget("fake-notify", "chat-x", long)

	got := fp.uniqueChunks()
	if got != cronNotifyMaxChunks {
		t.Errorf("unique chunks attempted = %d, want %d (cap must truncate before the loop)", got, cronNotifyMaxChunks)
	}
}

// TestR236SEC15_NotifyTargetUnderCapSendsAll verifies the cap is a
// ceiling, not a floor: when SplitText yields fewer chunks than the
// cap, every chunk is delivered.
func TestR236SEC15_NotifyTargetUnderCapSendsAll(t *testing.T) {
	t.Parallel()
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{}
	storeFakeNotifySender(s, map[string]platform.Platform{"fake-notify": fp})
	// 3 chunks (< cap=5).
	short := buildDistinctChunks(3, 8)
	chunks := platform.SplitText(short, 8)
	if len(chunks) >= cronNotifyMaxChunks {
		t.Fatalf("test fixture: expected fewer chunks than cap, got %d (cap=%d)", len(chunks), cronNotifyMaxChunks)
	}
	s.notifyTarget("fake-notify", "chat-x", short)
	if got := fp.uniqueChunks(); got != len(chunks) {
		t.Errorf("unique chunks attempted = %d, want %d (under-cap path must send all)", got, len(chunks))
	}
}
