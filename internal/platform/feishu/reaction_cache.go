// Reaction-id cache primitives; the cache itself lives on Feishu.reactionIDs
// and is swept by cleanupReactionsTick.
package feishu

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/platform"
)

// reactionCacheTTL bounds how long an unpaired reactionIDs entry lingers. 12h
// comfortably exceeds any live queued-message lifespan, so a real
// RemoveReaction still hits the cache; older entries are orphans safe to GC.
const reactionCacheTTL = 12 * time.Hour

// reactionCacheEntry is the sync.Map value shape for reactionIDs.
type reactionCacheEntry struct {
	id     string
	expiry int64 // UnixNano; expired when now >= expiry (boundary-inclusive, matches cleanupReactionsTick)
}

// reactionEmojiType maps platform.ReactionType to a Feishu emoji_type; unknown
// types return "" so callers can skip.
func reactionEmojiType(r platform.ReactionType) string {
	switch r {
	case platform.ReactionQueued:
		// HOURGLASS hints "waiting" without implying success or failure.
		return "HOURGLASS"
	}
	return ""
}

// reactionCacheKey builds the (msgID, emojiType) composite key for reactionIDs.
func reactionCacheKey(messageID, emojiType string) string {
	return messageID + "|" + emojiType
}

// reactionCleanupInterval is how often expired reactionIDs entries are swept.
const reactionCleanupInterval = 150 * time.Second

func (f *Feishu) cleanupReactions(ctx context.Context) {
	ticker := time.NewTicker(reactionCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// The recover frame lives in the per-tick helper so a panic does
			// not end the loop.
			f.cleanupReactionsTick()
		case <-ctx.Done():
			return
		}
	}
}

// cleanupReactionsTick drops expired (or malformed) reactionIDs entries.
func (f *Feishu) cleanupReactionsTick() {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicRecoveredTotal.Add(1)
			slog.Error("feishu: cleanupReactions tick panic recovered; retrying on next tick",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	now := time.Now().UnixNano()
	f.reactionIDs.Range(func(k, v any) bool {
		entry, ok := v.(reactionCacheEntry)
		if !ok || entry.expiry <= now {
			f.reactionIDs.Delete(k)
		}
		return true
	})
}
