package feishu

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestCleanupNoncesTick_SweepsExpiredReactionEntries exercises R175-P1: an
// orphaned reactionIDs entry (Add without a paired Remove) whose expiry has
// passed MUST be purged by the cleanup tick, and a still-live entry MUST be
// kept. Without this sweep the map would grow unbounded across bot-restart
// / message-deleted / early-exit paths in the dispatch layer.
func TestCleanupNoncesTick_SweepsExpiredReactionEntries(t *testing.T) {
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

	f.cleanupNoncesTick()

	if _, ok := f.reactionIDs.Load(expiredKey); ok {
		t.Error("expired reactionIDs entry must be dropped by cleanupNoncesTick")
	}
	if _, ok := f.reactionIDs.Load(liveKey); !ok {
		t.Error("live reactionIDs entry must NOT be dropped — TTL not yet reached")
	}
}

// TestCleanupNoncesTick_DropsMalformedReactionEntry — a future refactor that
// accidentally stores a different value type under reactionIDs must not wedge
// RemoveReaction forever; the defensive type assertion in the sweep should
// drop such entries so the map self-heals.
func TestCleanupNoncesTick_DropsMalformedReactionEntry(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	f.reactionIDs.Store(reactionCacheKey("msg-bad", "HOURGLASS"), "legacy-raw-string")

	f.cleanupNoncesTick()

	if _, ok := f.reactionIDs.Load(reactionCacheKey("msg-bad", "HOURGLASS")); ok {
		t.Error("malformed reactionIDs entry (wrong value type) must be dropped")
	}
}

// TestReactionCacheTTL_ExceedsSessionTTL documents the invariant called out in
// the production comment: the cache TTL MUST exceed the typical session TTL
// (default 30 minutes in config) so any legitimate live RemoveReaction still
// hits a cached entry. If a future edit shortens reactionCacheTTL below that
// window, this guard fails loudly.
func TestReactionCacheTTL_ExceedsSessionTTL(t *testing.T) {
	t.Parallel()
	const defaultSessionTTL = 30 * time.Minute
	if reactionCacheTTL <= defaultSessionTTL {
		t.Errorf("reactionCacheTTL = %v; must exceed the default session ttl (%v) "+
			"so orphan-free Add/Remove pairs always hit a cached entry",
			reactionCacheTTL, defaultSessionTTL)
	}
	if reactionCacheTTL > 7*24*time.Hour {
		t.Errorf("reactionCacheTTL = %v; a week+ window is too long for "+
			"best-effort feedback and risks bloat — tighten or justify",
			reactionCacheTTL)
	}
}

// TestReactionCache_CleanupContract is a source-level regression gate ensuring
// the sweep in cleanupNoncesTick actually ranges over reactionIDs. A refactor
// that accidentally drops the second Range (e.g. by splitting the tick into a
// helper that only touches seenNonces) would silently remove the TTL sweep;
// this test keeps the contract visible even when no panic or data corruption
// is triggered at runtime.
func TestReactionCache_CleanupContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("feishu.go")
	if err != nil {
		t.Fatalf("read feishu.go: %v", err)
	}
	src := string(data)

	tickIdx := strings.Index(src, "func (f *Feishu) cleanupNoncesTick()")
	if tickIdx < 0 {
		t.Fatal("cleanupNoncesTick helper missing")
	}
	tickEnd := strings.Index(src[tickIdx:], "\n}\n")
	if tickEnd < 0 {
		t.Fatal("cleanupNoncesTick body not terminated")
	}
	tickBody := src[tickIdx : tickIdx+tickEnd]

	for _, anchor := range []string{
		"f.reactionIDs.Range(",
		"reactionCacheEntry",
		"entry.expiry",
		"f.reactionIDs.Delete(",
	} {
		if !strings.Contains(tickBody, anchor) {
			t.Errorf("cleanupNoncesTick body missing anchor %q — R175-P1 "+
				"reactionIDs TTL sweep may have regressed", anchor)
		}
	}
}
