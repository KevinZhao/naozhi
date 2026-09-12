package feishu

import (
	"cmp"
	"context"
	"log/slog"
	"runtime/debug"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// nonce.go — webhook replay protection: the seen-nonce set, its TTL sweep and
// the bounded eviction that keeps a flood from growing it without limit.
// Extracted from feishu.go (J10 of #2548).

const nonceTTL = 5 * time.Minute

// maxSeenNonces caps the replay map (~3.6 MB at 50k entries) so a flood of
// authenticated unique-nonce requests cannot bloat memory.
const maxSeenNonces = 50000

// nonceEvictionBatch is how many oldest entries evictOldestNonces removes on
// cap-hit. Without it a leaked verification_token could pin the map at cap
// and 429 every legitimate webhook for nonceTTL; ~2% of cap keeps a sustained
// flood paying a steady cost instead of thrashing every insert (#1332).
const nonceEvictionBatch = 1024

// tokenFailCooldown bounds how long a failed token refresh is cached: 5s
// balances operator-visible recovery with upstream rate protection.
const tokenFailCooldown = 5 * time.Second

// nonceCleanupInterval is nonceTTL/2 so an expired entry never lingers past
// ~1.5 × TTL and seenNoncesCount does not creep toward the cap under
// sustained traffic.
const nonceCleanupInterval = nonceTTL / 2

func (f *Feishu) cleanupNonces(ctx context.Context) {
	ticker := time.NewTicker(nonceCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// The recover frame MUST live in the per-tick helper, not here: a
			// function-scope recover would unwind out of the loop and disable
			// replay protection until restart.
			f.cleanupNoncesTick()
		case <-ctx.Done():
			return
		}
	}
}

// cleanupNoncesTick performs one sweep of the expired-nonce map in its own
// recover frame so a panic is logged and the next tick retries.
func (f *Feishu) cleanupNoncesTick() {
	defer func() {
		if r := recover(); r != nil {
			metrics.PanicRecoveredTotal.Add(1)
			slog.Error("feishu: cleanupNonces tick panic recovered; replay protection continues on next tick",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	// One wall-time basis for both sweeps so cutoff decisions are consistent.
	nowT := time.Now()
	now := nowT.Unix()
	deleted := int64(0)
	f.seenNonces.Range(func(k, v any) bool {
		// sync.Map has no type safety; drop malformed entries so the map recovers.
		ts, ok := v.(int64)
		if !ok || ts < now {
			f.seenNonces.Delete(k)
			deleted++
		}
		return true
	})
	if deleted > 0 {
		// Clamp at zero: malformed entries deleted above may have bypassed the
		// counted insert path, and a negative counter would eventually 429
		// legitimate traffic.
		if n := f.seenNoncesCount.Add(-deleted); n < 0 {
			f.seenNoncesCount.Store(0)
		}
	}

	// reactionIDs sweep rides the same tick (UnixNano: its TTL is finer).
	nowNano := nowT.UnixNano()
	f.reactionIDs.Range(func(k, v any) bool {
		entry, ok := v.(reactionCacheEntry)
		if !ok || entry.expiry <= nowNano {
			f.reactionIDs.Delete(k)
		}
		return true
	})
}

// evictNonces dispatches to the test hook when set, else evictOldestNonces.
func (f *Feishu) evictNonces() int {
	if f.evictNoncesFn != nil {
		return f.evictNoncesFn()
	}
	return f.evictOldestNonces()
}

// evictOldestNonces removes up to nonceEvictionBatch entries closest to
// natural expiry so fresh traffic keeps flowing when a leaked-token flood
// hits the cap (#1332). The replay regression is bounded: only nonces inside
// the current nonceTTL window are evicted, and timestamp verification still
// rejects older payloads. Returns the number actually removed.
func (f *Feishu) evictOldestNonces() int {
	// Serialized so overlapping evictions cannot split the counter decrement
	// and lapse the cap guard; the counter is resynced to the live size on exit (#1534).
	f.nonceEvictMu.Lock()
	defer f.nonceEvictMu.Unlock()

	type nonceEntry struct {
		key    any
		expiry int64
	}
	// Inserts missed by the Range are necessarily fresh, so excluding them only
	// makes eviction more aggressive on the genuinely-old set.
	entries := make([]nonceEntry, 0, nonceEvictionBatch*2)
	f.seenNonces.Range(func(k, v any) bool {
		ts, ok := v.(int64)
		if !ok {
			// Cross-type entry: sentinel expiry sorts it to the front.
			entries = append(entries, nonceEntry{key: k, expiry: 0})
			return true
		}
		entries = append(entries, nonceEntry{key: k, expiry: ts})
		return true
	})
	if len(entries) == 0 {
		return 0
	}
	slices.SortFunc(entries, func(a, b nonceEntry) int {
		return cmp.Compare(a.expiry, b.expiry)
	})
	limit := nonceEvictionBatch
	if limit > len(entries) {
		limit = len(entries)
	}
	deleted := 0
	for i := 0; i < limit; i++ {
		if _, loaded := f.seenNonces.LoadAndDelete(entries[i].key); loaded {
			deleted++
		}
	}
	if deleted > 0 {
		// Resync to the live size rather than Add(-deleted): concurrent inserts
		// and the cleanup ticker still adjust the counter, and a relative
		// decrement could drift below the real map size. Only runs on cap-hit.
		live := int64(0)
		f.seenNonces.Range(func(_, _ any) bool {
			live++
			return true
		})
		f.seenNoncesCount.Store(live)
	}
	return deleted
}
