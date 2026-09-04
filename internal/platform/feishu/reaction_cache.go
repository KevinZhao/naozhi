// Reaction-id cache primitives; the cache itself lives on Feishu.reactionIDs
// and is swept by cleanupNoncesTick.
package feishu

import (
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// reactionCacheTTL bounds how long an unpaired reactionIDs entry lingers. 12h
// comfortably exceeds any live queued-message lifespan, so a real
// RemoveReaction still hits the cache; older entries are orphans safe to GC.
const reactionCacheTTL = 12 * time.Hour

// reactionCacheEntry is the sync.Map value shape for reactionIDs.
type reactionCacheEntry struct {
	id     string
	expiry int64 // UnixNano; expired when now >= expiry (boundary-inclusive, matches cleanupNoncesTick)
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
