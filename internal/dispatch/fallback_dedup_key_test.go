package dispatch

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestFallbackDedupKey_StableWithinMinute pins the dedup contract for
// adapter-side empty-EventID retries (#1310). Two events with identical
// (Platform, ChatID, MessageID) within the same wall-clock minute must
// produce the same fallback key so platform.Dedup.Seen short-circuits the
// second pass — without this, a Feishu webhook retry whose adapter dropped
// event_id would re-enter BuildHandler, double-charge tokens, and queue
// "正在处理上一条消息" noise.
func TestFallbackDedupKey_StableWithinMinute(t *testing.T) {
	t.Parallel()
	msg := platform.IncomingMessage{
		Platform:  "feishu",
		ChatID:    "chat-1",
		MessageID: "msg-A",
	}
	t0 := time.Date(2026, 5, 27, 13, 30, 5, 0, time.UTC)
	t1 := t0.Add(50 * time.Second) // still inside the same UTC minute
	a := fallbackDedupKey(msg, t0)
	b := fallbackDedupKey(msg, t1)
	if a != b {
		t.Errorf("same-minute keys differ: %q vs %q", a, b)
	}
	if a == "" {
		t.Error("fallback key must be non-empty so Dedup.Seen records it")
	}
}

// TestFallbackDedupKey_SeparatesAcrossMinute confirms the minute bucket
// caps the dedup window — two distinct user messages with the same
// (Platform, ChatID, MessageID="") fields but a minute apart MUST hash
// to different keys so a fresh message after the bucket boundary is not
// silently dropped as a duplicate of the previous one.
func TestFallbackDedupKey_SeparatesAcrossMinute(t *testing.T) {
	t.Parallel()
	msg := platform.IncomingMessage{Platform: "feishu", ChatID: "c", MessageID: ""}
	t0 := time.Date(2026, 5, 27, 13, 30, 59, 0, time.UTC)
	t1 := t0.Add(2 * time.Second) // crosses minute boundary into 13:31:01
	if a, b := fallbackDedupKey(msg, t0), fallbackDedupKey(msg, t1); a == b {
		t.Errorf("keys across minute boundary collided: %q == %q", a, b)
	}
}

// TestFallbackDedupKey_DistinguishesFields rules out the degenerate
// implementation that hashes only (Platform, minute) and ignores the
// per-chat / per-message tuple. Distinct ChatIDs must yield distinct
// keys even when everything else matches; same for MessageID.
func TestFallbackDedupKey_DistinguishesFields(t *testing.T) {
	t.Parallel()
	now := time.Unix(1748352000, 0) // arbitrary fixed minute
	base := platform.IncomingMessage{Platform: "feishu", ChatID: "c1", MessageID: "m1"}
	a := fallbackDedupKey(base, now)
	if b := fallbackDedupKey(platform.IncomingMessage{Platform: "feishu", ChatID: "c2", MessageID: "m1"}, now); a == b {
		t.Errorf("ChatID change did not affect key: %q", a)
	}
	if b := fallbackDedupKey(platform.IncomingMessage{Platform: "feishu", ChatID: "c1", MessageID: "m2"}, now); a == b {
		t.Errorf("MessageID change did not affect key: %q", a)
	}
	if b := fallbackDedupKey(platform.IncomingMessage{Platform: "weixin", ChatID: "c1", MessageID: "m1"}, now); a == b {
		t.Errorf("Platform change did not affect key: %q", a)
	}
}

// TestFallbackDedupKey_PrefixedNamespace pins the "fallback:" prefix so
// real EventIDs (which never start with "fallback:") cannot collide with
// the synthetic namespace. The prefix is part of the wire contract for
// the dedup map; downstream operators inspecting the cache should be
// able to spot fallback entries at a glance.
func TestFallbackDedupKey_PrefixedNamespace(t *testing.T) {
	t.Parallel()
	got := fallbackDedupKey(platform.IncomingMessage{Platform: "p", ChatID: "c", MessageID: "m"}, time.Unix(0, 0))
	if got[:9] != "fallback:" {
		t.Errorf("expected key to start with 'fallback:', got %q", got)
	}
}
