// Reaction-id caching primitives extracted from feishu.go (R214-ARCH-13
// continuation of the file split). Holds the TTL constant, the sync.Map
// value shape, the platform→emoji_type mapping, and the (msgID,emoji)
// composite-key helper.
//
// The actual cache lives on Feishu.reactionIDs (sync.Map keyed by
// reactionCacheKey) and is swept by cleanupNoncesTick — those owners stay
// in feishu.go for now since they hold receiver state. Splitting only the
// pure-helper layer here is enough to drop the per-file noise that motivated
// R214-ARCH-13 without forcing a structural rewrite of cleanup tick scheduling.
//
// No behavior change: every name preserves its package-level identity, and
// existing tests in reaction_cache_ttl_test.go / cleanup_nonces_test.go
// continue to compile and pass against the same surface.
package feishu

import (
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// reactionCacheTTL bounds how long an unpaired reactionIDs entry lingers
// before the cleanup sweep drops it. Add-without-Remove windows come from:
// (a) bot restart between the two calls (rare; queue processing is short);
// (b) the Feishu user deleting the message out from under us; (c) an early
// return in the dispatch path before RemoveReaction fires. 12h comfortably
// exceeds the session ttl default (30m) and the longest reasonable "queued"
// lifespan a message might have, so any live RemoveReaction still hits a
// cached entry; anything older than 12h is almost certainly orphaned and
// safe to GC. R175-P1.
const reactionCacheTTL = 12 * time.Hour

// reactionCacheEntry is the sync.Map value shape for reactionIDs. Kept as a
// struct (not a raw string) so the expiry can be checked without consulting
// any external state. R175-P1.
type reactionCacheEntry struct {
	id     string
	expiry int64 // UnixNano; expired when time.Now().UnixNano() >= expiry (boundary-inclusive, matches sweep at cleanupNoncesTick)
}

// reactionEmojiType maps platform-agnostic ReactionType to Feishu emoji_type.
// Feishu's reaction API uses string emoji_types (see OpenAPI docs). Unknown
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
