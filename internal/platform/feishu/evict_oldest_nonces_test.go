package feishu

import (
	"strconv"
	"testing"
)

// TestEvictOldestNonces_RemovesOldestBatch pins R20260527122801-SEC-8 (#1332):
// when the seenNonces map sits at maxSeenNonces under attack, calling
// evictOldestNonces must remove up to nonceEvictionBatch entries chosen by
// smallest expiry timestamp so legitimate webhooks regain insert headroom
// before nonceTTL elapses.
func TestEvictOldestNonces_RemovesOldestBatch(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	// Seed the map with 3*batch entries so we can prove the SMALLEST batch
	// is chosen, not just an arbitrary slice.
	total := nonceEvictionBatch * 3
	for i := 0; i < total; i++ {
		key := "k-" + strconv.Itoa(i)
		// expiry == i means lower i ⇒ older entry ⇒ evicted first.
		f.seenNonces.Store(key, int64(i))
	}
	f.seenNoncesCount.Store(int64(total))

	deleted := f.evictOldestNonces()
	if deleted != nonceEvictionBatch {
		t.Errorf("evictOldestNonces returned %d; want %d", deleted, nonceEvictionBatch)
	}
	if got := f.seenNoncesCount.Load(); got != int64(total-nonceEvictionBatch) {
		t.Errorf("seenNoncesCount = %d; want %d", got, total-nonceEvictionBatch)
	}

	// The smallest-expiry keys (k-0 .. k-(batch-1)) MUST all be gone.
	for i := 0; i < nonceEvictionBatch; i++ {
		key := "k-" + strconv.Itoa(i)
		if _, ok := f.seenNonces.Load(key); ok {
			t.Errorf("key %s should have been evicted (smallest expiry)", key)
			break
		}
	}
	// At least one of the largest-expiry keys MUST still be present —
	// negative control so a future "evict everything" regression is caught.
	survivor := "k-" + strconv.Itoa(total-1)
	if _, ok := f.seenNonces.Load(survivor); !ok {
		t.Errorf("key %s should have survived (largest expiry)", survivor)
	}
}

// TestEvictOldestNonces_EmptyMapNoop verifies the helper is safe on an empty
// map (e.g. a race where the cleanup ticker just swept everything between
// the cap-check and our eviction call).
func TestEvictOldestNonces_EmptyMapNoop(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	if got := f.evictOldestNonces(); got != 0 {
		t.Errorf("empty map evict = %d; want 0", got)
	}
	if got := f.seenNoncesCount.Load(); got != 0 {
		t.Errorf("counter should stay at 0, got %d", got)
	}
}

// TestEvictOldestNonces_HandlesMalformedValue covers the defensive sentinel
// branch: a sync.Map entry with a non-int64 value (future refactor risk)
// should be evicted preferentially because we sort it to expiry=0.
func TestEvictOldestNonces_HandlesMalformedValue(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	// One malformed entry plus a few well-formed ones with high expiry.
	f.seenNonces.Store("bad", "not-an-int64")
	for i := 0; i < 5; i++ {
		f.seenNonces.Store("good-"+strconv.Itoa(i), int64(1<<60+i))
	}
	f.seenNoncesCount.Store(6)

	deleted := f.evictOldestNonces()
	if deleted < 1 {
		t.Errorf("evict should have removed at least the malformed entry, got %d", deleted)
	}
	// Malformed entry must be gone.
	if _, ok := f.seenNonces.Load("bad"); ok {
		t.Error("malformed entry should have been preferentially evicted")
	}
}

// TestEvictOldestNonces_CounterClampsAtZero — if external state drift makes
// the counter smaller than the deletion count, the helper must clamp to 0
// rather than going negative (consistent with cleanupNoncesTick).
func TestEvictOldestNonces_CounterClampsAtZero(t *testing.T) {
	t.Parallel()
	f := &Feishu{}
	f.seenNonces.Store("k", int64(1))
	f.seenNoncesCount.Store(0) // intentional drift

	f.evictOldestNonces()

	if got := f.seenNoncesCount.Load(); got != 0 {
		t.Errorf("counter should be clamped to 0, got %d", got)
	}
}
