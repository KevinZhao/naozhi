package feishu

import (
	"testing"
	"time"
)

// TestCleanupReactionsTick_SweepsExpiredReactionEntries exercises R175-P1: an
// orphaned reactionIDs entry (Add without a paired Remove) whose expiry has
// passed MUST be purged by the cleanup tick, and a still-live entry MUST be
// kept. Without this sweep the map would grow unbounded across bot-restart
// / message-deleted / early-exit paths in the dispatch layer.
func TestCleanupReactionsTick_SweepsExpiredReactionEntries(t *testing.T) {
	t.Parallel()
	f := &Feishu{}

	// Seed two entries directly — production would set these via the
	// AddReaction HTTP success branch, but the unit test wants to isolate
	// the cleanup logic from network I/O.
	expiredKey := reactionCacheKey("msg-expired", "HOURGLASS")
	liveKey := reactionCacheKey("msg-live", "HOURGLASS")

	f.reactionIDs.Store(expiredKey, reactionCacheEntry{
		id:     "r-expired",
		expiry: time.Now().Add(-time.Hour).UnixNano(), // already past
	})
	f.reactionIDs.Store(liveKey, reactionCacheEntry{
		id:     "r-live",
		expiry: time.Now().Add(time.Hour).UnixNano(), // still fresh
	})

	f.cleanupReactionsTick()

	if _, ok := f.reactionIDs.Load(expiredKey); ok {
		t.Error("expired reactionIDs entry must be dropped by cleanupReactionsTick")
	}
	if _, ok := f.reactionIDs.Load(liveKey); !ok {
		t.Error("live reactionIDs entry must NOT be dropped — TTL not yet reached")
	}
}
