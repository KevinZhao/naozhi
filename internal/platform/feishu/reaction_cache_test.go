package feishu

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestReactionCacheKey pins the wire-format invariant that the composite key
// is messageID + "|" + emojiType. The cache is keyed by this exact string in
// AddReaction/RemoveReaction, so any drift here would silently produce
// orphan entries (Add stores at key A, Remove probes at key B). The pipe
// separator is chosen because Feishu message IDs are not allowed to
// contain it (they are URL-safe base62 prefixed with "om_"), so collision
// across (msgID, emojiType) pairs is structurally impossible. R214-ARCH-13.
func TestReactionCacheKey(t *testing.T) {
	t.Parallel()
	got := reactionCacheKey("om_abc123", "HOURGLASS")
	want := "om_abc123|HOURGLASS"
	if got != want {
		t.Errorf("reactionCacheKey = %q, want %q", got, want)
	}
	// Empty emoji slot must still produce a parseable key (separator
	// preserved) so an "" cache miss still hits the right shape rather
	// than colliding with a different msgID's empty-emoji slot.
	if reactionCacheKey("a", "") == reactionCacheKey("a|", "") {
		t.Errorf("empty-emoji key collides with msgID containing %q",
			"|")
	}
}

// TestReactionEmojiType pins the Feishu-side emoji_type strings. The Feishu
// reactions API rejects unknown emoji_types with a 400, so silently
// returning "" for an unmapped ReactionType lets callers skip the API
// round-trip rather than dispatching a guaranteed-failure request. Adding a
// new platform.ReactionType without extending this switch should be caught
// by the default-empty branch + the queued case below; if the queued
// mapping ever needs to change (e.g. Feishu retiring HOURGLASS), the test
// fails loudly so the wire-format change is reviewed deliberately.
func TestReactionEmojiType(t *testing.T) {
	t.Parallel()
	if got := reactionEmojiType(platform.ReactionQueued); got != "HOURGLASS" {
		t.Errorf("reactionEmojiType(Queued) = %q, want HOURGLASS", got)
	}
	if got := reactionEmojiType(platform.ReactionType("nonexistent")); got != "" {
		t.Errorf("reactionEmojiType(unknown) = %q, want empty (API skip signal)", got)
	}
}
