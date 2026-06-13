package session

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestStoreMarshalBufRecyclable pins the cap-gate decision (R20260613-PERF-3 /
// #2073): buffers up to and including the cap are recyclable; anything larger is
// dropped so a one-off oversized store cannot permanently pin a large array.
// Testing the pure predicate (rather than a sync.Pool round-trip) keeps this
// deterministic — sync.Pool gives no Get-returns-Put guarantee under GC, which
// -race triggers aggressively.
func TestStoreMarshalBufRecyclable(t *testing.T) {
	cases := []struct {
		capacity int
		want     bool
	}{
		{0, true},
		{4096, true},
		{storeMarshalBufMaxCap - 1, true},
		{storeMarshalBufMaxCap, true},      // exactly at cap → recyclable
		{storeMarshalBufMaxCap + 1, false}, // one over → dropped
		{storeMarshalBufMaxCap * 4, false},
	}
	for _, c := range cases {
		if got := storeMarshalBufRecyclable(c.capacity); got != c.want {
			t.Errorf("storeMarshalBufRecyclable(%d) = %v, want %v", c.capacity, got, c.want)
		}
	}
}

// TestPutStoreMarshalBuf_DoesNotPanic verifies putStoreMarshalBuf handles both
// recyclable and oversized buffers without panicking (it must not, e.g., slice
// an oversized buffer it intends to drop). It does NOT assert pool retrieval —
// sync.Pool may evict on GC, so a "Get returns my Put" assertion would be flaky.
func TestPutStoreMarshalBuf_DoesNotPanic(t *testing.T) {
	putStoreMarshalBuf(make([]byte, 9, 4096))                     // under cap, non-empty
	putStoreMarshalBuf(make([]byte, 0, storeMarshalBufMaxCap))    // exactly at cap
	putStoreMarshalBuf(make([]byte, 10, storeMarshalBufMaxCap*2)) // oversized → dropped
	putStoreMarshalBuf(nil)                                       // zero-value buffer
}

// TestMarshalStoreEntries_ReusedBufferProducesCorrectOutput is the correctness
// guard for buffer reuse: marshal twice across a saveStore (which returns the
// buffer to the pool), with the SECOND store smaller than the first, and assert
// the second output is not contaminated by leftover bytes from the first. This
// catches a missing [:0] reset or a stale-length bug in the pool path.
func TestMarshalStoreEntries_ReusedBufferProducesCorrectOutput(t *testing.T) {
	dir := t.TempDir()

	// First save: several sessions → grows the pooled buffer.
	big := map[string]*ManagedSession{}
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		s := newSessionWithID("feishu:direct:"+id+":general", "sess-"+id)
		s.SetUserLabel("label-" + id + "-padding-to-make-the-buffer-grow-wider")
		big[s.key] = s
	}
	if err := saveStore(filepath.Join(dir, "big.json"), big); err != nil {
		t.Fatalf("saveStore big: %v", err)
	}

	// Second save: a single tiny session. If the pooled buffer is reused
	// without a proper reset, leftover bytes from the big store would leak in.
	one := newSessionWithID("feishu:direct:solo:general", "sess-solo")
	small := map[string]*ManagedSession{one.key: one}
	smallPath := filepath.Join(dir, "small.json")
	if err := saveStore(smallPath, small); err != nil {
		t.Fatalf("saveStore small: %v", err)
	}

	loaded := loadStore(smallPath)
	if len(loaded) != 1 {
		t.Fatalf("small store loaded %d entries, want 1 (buffer reuse leaked stale entries?)", len(loaded))
	}
	if _, ok := loaded[one.key]; !ok {
		t.Errorf("small store missing key %q", one.key)
	}

	// Direct marshal twice in a row through the pool, validating each result is
	// well-formed JSON of the expected size.
	first, err := marshalStoreEntries(big)
	if err != nil {
		t.Fatalf("marshalStoreEntries big: %v", err)
	}
	var es []storeEntry
	if err := json.Unmarshal(first, &es); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if len(es) != 4 {
		t.Errorf("first marshal entries = %d, want 4", len(es))
	}
	putStoreMarshalBuf(first)

	second, err := marshalStoreEntries(small)
	if err != nil {
		t.Fatalf("marshalStoreEntries small: %v", err)
	}
	es = nil
	if err := json.Unmarshal(second, &es); err != nil {
		t.Fatalf("unmarshal second (reused buffer corrupt?): %v", err)
	}
	if len(es) != 1 {
		t.Errorf("second marshal entries = %d, want 1", len(es))
	}
	putStoreMarshalBuf(second)
}

// TestMarshalStoreEntriesFunc_PoolReuseNoLeak marshals repeatedly through the
// pooled buffer with alternating large/small session sets, returning the buffer
// each time, and asserts every output is exactly the expected entry set. This is
// the deterministic correctness guard for buffer reuse (R20260613-PERF-3 /
// #2073): if a [:0] reset were missing, a small marshal that reuses a buffer
// previously grown by a large marshal would surface stale trailing bytes / a
// wrong entry count.
func TestMarshalStoreEntriesFunc_PoolReuseNoLeak(t *testing.T) {
	mk := func(ids ...string) []*ManagedSession {
		out := make([]*ManagedSession, 0, len(ids))
		for _, id := range ids {
			out = append(out, newSessionWithID("feishu:direct:"+id+":general", "sess-"+id))
		}
		return out
	}
	large := mk("a", "b", "c", "d", "e")
	small := mk("z")

	countEntries := func(raw []byte) int {
		var es []storeEntry
		if err := json.Unmarshal(raw, &es); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
		return len(es)
	}

	// Interleave so a reused (previously grown) buffer is exercised by a smaller
	// set, repeatedly, across many iterations.
	for i := 0; i < 20; i++ {
		b, err := marshalStoreEntriesSlice(large)
		if err != nil {
			t.Fatalf("marshal large: %v", err)
		}
		if n := countEntries(b); n != len(large) {
			t.Fatalf("iter %d large: got %d entries, want %d (stale buffer leak?)", i, n, len(large))
		}
		putStoreMarshalBuf(b)

		b, err = marshalStoreEntriesSlice(small)
		if err != nil {
			t.Fatalf("marshal small: %v", err)
		}
		if n := countEntries(b); n != len(small) {
			t.Fatalf("iter %d small: got %d entries, want %d (stale buffer leak from prior large marshal?)", i, n, len(small))
		}
		putStoreMarshalBuf(b)
	}
}

// TestMarshalStoreEntries_AllocsDoNotScaleWithN proves the assembly buffer is no
// longer allocated per save. Before the pool, the buffer was
// make([]byte, 0, 256*N) — a fresh O(N×256) heap allocation on EVERY save, so
// steady-state allocs/op grew with the session count. With the pool the buffer
// is borrowed/returned, so allocs/op must NOT scale with N.
//
// Skipped under the race detector: -race rewrites allocation paths and the
// shared sync.Pool is drained by concurrently-running tests, making
// testing.AllocsPerRun non-deterministic. The reuse invariant is covered
// deterministically by TestMarshalStoreEntriesFunc_PoolReuseNoLeak above;
// this test adds the allocation-scaling guard for the normal (non-race) run.
func TestMarshalStoreEntries_AllocsDoNotScaleWithN(t *testing.T) {
	if raceEnabled {
		t.Skip("AllocsPerRun is unreliable under -race + shared sync.Pool; reuse covered by PoolReuseNoLeak")
	}
	build := func(n int) map[string]*ManagedSession {
		m := map[string]*ManagedSession{}
		for i := 0; i < n; i++ {
			id := string(rune('a'+i%26)) + string(rune('0'+i/26))
			s := newSessionWithID("feishu:direct:"+id+":general", "sess-"+id)
			s.SetUserLabel("a-reasonably-wide-label-to-exercise-buffer-growth-" + id)
			m[s.key] = s
		}
		return m
	}

	measure := func(sessions map[string]*ManagedSession) float64 {
		// Prime the per-session marshal caches AND grow the pooled buffer to its
		// steady-state size so the measured loop hits the reuse fast path.
		for i := 0; i < 50; i++ {
			b, _ := marshalStoreEntries(sessions)
			putStoreMarshalBuf(b)
		}
		return testing.AllocsPerRun(100, func() {
			b, _ := marshalStoreEntries(sessions)
			putStoreMarshalBuf(b)
		})
	}

	small := measure(build(2))
	large := measure(build(80)) // ~80*256 = 20KB buffer pre-pool; would alloc per call

	// Allocs must be small and constant regardless of N (remaining allocs are
	// the fixed-cost map-range iterator closures, not the assembly buffer). A
	// regression to make-per-save would push large-N far above this.
	const ceiling = 6
	if small > ceiling {
		t.Errorf("small-N allocs/op = %v, want <= %d", small, ceiling)
	}
	if large > ceiling {
		t.Errorf("large-N allocs/op = %v, want <= %d (assembly buffer allocated per save? pool regressed)", large, ceiling)
	}
}
