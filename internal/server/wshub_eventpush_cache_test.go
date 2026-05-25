package server

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
)

// TestHistoryMarshalCache_HitOnRepeatFingerprint locks the R214-PERF-4 fix:
// when N eventPushLoop goroutines fan out the same (key, lastTime, entries)
// payload (multi-tab dashboard on one session), getOrMarshal must invoke
// the marshal callback exactly once and return identical bytes to the rest.
//
// Pre-fix path called marshalPooled per subscriber unconditionally, paying
// the JSON reflect cost N times for a payload that was byte-identical
// between subscribers. With this regression test in place, removing the
// cache (or breaking the fingerprint match logic) flips marshalCount > 1.
func TestHistoryMarshalCache_HitOnRepeatFingerprint(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	entries := []cli.EventEntry{
		{Time: 100, Type: "text", Summary: "first"},
		{Time: 200, Type: "tool_use", Tool: "Read"},
	}
	const lastTime int64 = 50
	const key = "feishu:p2p:user-aaa"

	var marshalCount int
	marshal := func() ([]byte, error) {
		marshalCount++
		return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
	}

	first, hit, err := cache.getOrMarshal(key, lastTime, entries, marshal)
	if err != nil {
		t.Fatalf("first getOrMarshal: %v", err)
	}
	if hit {
		t.Fatal("first call must miss the cache")
	}
	if marshalCount != 1 {
		t.Fatalf("first call marshal invocations = %d, want 1", marshalCount)
	}

	// Simulate three more subscribers fanning out the same notify wave.
	for i := 0; i < 3; i++ {
		got, hit, err := cache.getOrMarshal(key, lastTime, entries, marshal)
		if err != nil {
			t.Fatalf("subscriber #%d getOrMarshal: %v", i+2, err)
		}
		if !hit {
			t.Fatalf("subscriber #%d expected cache hit (regression: marshal coalesce broken)", i+2)
		}
		if string(got) != string(first) {
			t.Fatalf("subscriber #%d bytes drift\n  got=%s\n want=%s", i+2, got, first)
		}
	}
	if marshalCount != 1 {
		t.Fatalf("total marshal invocations = %d, want 1 (R214-PERF-4: coalesce N->1)", marshalCount)
	}

	// Bytes must round-trip back to a structurally-equal ServerMsg so the
	// dashboard sees the same shape it used to. R241-SEC-13 / R229-PERF-4
	// style contract: cached []byte is the canonical wire form.
	var decoded node.ServerMsg
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("decode cached bytes: %v", err)
	}
	if decoded.Type != "history" || decoded.Key != key || len(decoded.Events) != len(entries) {
		t.Fatalf("decoded shape mismatch: %+v", decoded)
	}
}

// TestHistoryMarshalCache_MissOnFingerprintDrift verifies the cache does NOT
// hand back stale bytes when a subscriber's cursor (lastTime) has drifted
// from the cached fingerprint — e.g. a slow tab that fell behind the head
// and is now catching up with a different `since` window. The cache must
// re-marshal so the bytes match this subscriber's actual entries tail, not
// the previous one.
func TestHistoryMarshalCache_MissOnFingerprintDrift(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	tail1 := []cli.EventEntry{{Time: 100, Type: "text"}}
	tail2 := []cli.EventEntry{{Time: 100, Type: "text"}, {Time: 200, Type: "result"}}
	const key = "feishu:p2p:user-bbb"

	var marshalCount int
	makeMarshal := func(entries []cli.EventEntry) func() ([]byte, error) {
		return func() ([]byte, error) {
			marshalCount++
			return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
		}
	}

	if _, _, err := cache.getOrMarshal(key, 50, tail1, makeMarshal(tail1)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Different lastTime → fingerprint mismatch → must miss.
	if _, hit, _ := cache.getOrMarshal(key, 99, tail1, makeMarshal(tail1)); hit {
		t.Fatal("lastTime drift must miss the cache")
	}
	// Different latest entry Time → fingerprint mismatch → must miss.
	if _, hit, _ := cache.getOrMarshal(key, 50, tail2, makeMarshal(tail2)); hit {
		t.Fatal("latest-entry drift must miss the cache")
	}
	if marshalCount != 3 {
		t.Fatalf("marshal invocations = %d, want 3 (each fingerprint-mismatch must re-marshal)", marshalCount)
	}
}

// TestHistoryMarshalCache_PerKeyIsolation verifies two distinct session keys
// do not share cache slots — a hit on key A must never hand back bytes from
// key B (which would corrupt the fan-out by sending the wrong session's
// events to a subscriber).
func TestHistoryMarshalCache_PerKeyIsolation(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	entries := []cli.EventEntry{{Time: 1, Type: "text", Summary: "shared-tail"}}

	a, _, err := cache.getOrMarshal("feishu:p2p:user-A", 0, entries, func() ([]byte, error) {
		return marshalPooled(node.ServerMsg{Type: "history", Key: "feishu:p2p:user-A", Events: entries})
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	b, _, err := cache.getOrMarshal("feishu:p2p:user-B", 0, entries, func() ([]byte, error) {
		return marshalPooled(node.ServerMsg{Type: "history", Key: "feishu:p2p:user-B", Events: entries})
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}
	if string(a) == string(b) {
		t.Fatal("per-key isolation broken: A and B yielded identical bytes for distinct keys")
	}
}

// TestHistoryMarshalCache_DropFreesSlot verifies the drop hook releases the
// cache entry so abandoned session keys do not accumulate cached payloads
// across the lifetime of long-running Hubs.
func TestHistoryMarshalCache_DropFreesSlot(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	entries := []cli.EventEntry{{Time: 1}}
	const key = "k"

	var marshalCount int
	marshal := func() ([]byte, error) {
		marshalCount++
		return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
	}

	if _, _, err := cache.getOrMarshal(key, 0, entries, marshal); err != nil {
		t.Fatal(err)
	}
	cache.drop(key)
	// After drop, the next call must miss again (slot was freed).
	if _, hit, _ := cache.getOrMarshal(key, 0, entries, marshal); hit {
		t.Fatal("post-drop call must miss; cache.drop did not release the slot")
	}
	if marshalCount != 2 {
		t.Fatalf("marshal invocations = %d, want 2 (drop must force re-marshal)", marshalCount)
	}
}

// TestHistoryMarshalCache_ConcurrentFanOut stress-tests the per-key mutex
// under the N-tab fan-out scenario: 64 goroutines pile in on the same
// (key, lastTime, entries) fingerprint. The marshal callback must run at
// most once and every goroutine must observe the same bytes.
func TestHistoryMarshalCache_ConcurrentFanOut(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	entries := []cli.EventEntry{
		{Time: 1000, Type: "init"},
		{Time: 2000, Type: "thinking", Summary: "x"},
		{Time: 3000, Type: "text", Summary: "y"},
	}
	const lastTime int64 = 500
	const key = "concurrent"
	const N = 64

	var marshalCount int64
	marshal := func() ([]byte, error) {
		atomic.AddInt64(&marshalCount, 1)
		return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
	}

	var wg sync.WaitGroup
	results := make([][]byte, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			data, _, err := cache.getOrMarshal(key, lastTime, entries, marshal)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			results[idx] = data
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&marshalCount); got != 1 {
		t.Fatalf("marshal invocations = %d, want 1 (R214-PERF-4: 64-way fan-out must coalesce to 1)", got)
	}
	for i := 1; i < N; i++ {
		if string(results[i]) != string(results[0]) {
			t.Fatalf("goroutine %d bytes drift; coalesce broken under contention", i)
		}
	}
}

// TestHistoryMarshalCache_EmptyEntriesBypass guards the helper contract: an
// empty entries slice has no fingerprint (no latest Time, no count signal)
// and must bypass the cache entirely. The expected eventPushLoop call site
// already short-circuits empty tails, but the helper stays correct if a
// future caller forgets.
func TestHistoryMarshalCache_EmptyEntriesBypass(t *testing.T) {
	t.Parallel()
	cache := newHistoryMarshalCache()

	var marshalCount int
	marshal := func() ([]byte, error) {
		marshalCount++
		return []byte(`{"type":"history","key":"k","events":[]}`), nil
	}

	if _, hit, _ := cache.getOrMarshal("k", 0, nil, marshal); hit {
		t.Fatal("empty entries must always miss")
	}
	if _, hit, _ := cache.getOrMarshal("k", 0, []cli.EventEntry{}, marshal); hit {
		t.Fatal("empty entries (zero-len slice) must always miss")
	}
	if marshalCount != 2 {
		t.Fatalf("marshal invocations = %d, want 2 (each empty call bypasses cache)", marshalCount)
	}
}

// TestHub_MarshalHistoryFrame_NilCacheFallback documents that a hand-built
// Hub (no NewHub) without historyMarshalCache still produces correct bytes
// — the fallback path matches the pre-fix marshalPooled output exactly.
// Tests that construct a bare *Hub for narrow assertions rely on this.
func TestHub_MarshalHistoryFrame_NilCacheFallback(t *testing.T) {
	t.Parallel()
	h := &Hub{} // no cache
	entries := []cli.EventEntry{{Time: 1, Type: "text"}}
	got, err := h.marshalHistoryFrame("k", 0, entries)
	if err != nil {
		t.Fatalf("marshalHistoryFrame: %v", err)
	}
	want, err := marshalPooled(node.ServerMsg{Type: "history", Key: "k", Events: entries})
	if err != nil {
		t.Fatalf("marshalPooled: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("nil-cache fallback drift\n  got=%s\n want=%s", got, want)
	}
}

// TestHub_MarshalHistoryFrame_CoalescesWithCache mirrors the production wire-up:
// build a Hub with NewHub-style cache and verify two back-to-back fan-out calls
// (same fingerprint) collapse to a single marshalPooled invocation observable
// via byte-identity of the returned slice header. R214-PERF-4 contract.
func TestHub_MarshalHistoryFrame_CoalescesWithCache(t *testing.T) {
	t.Parallel()
	h := &Hub{historyMarshalCache: newHistoryMarshalCache()}
	entries := []cli.EventEntry{{Time: 1}, {Time: 2}}
	a, err := h.marshalHistoryFrame("kk", 0, entries)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.marshalHistoryFrame("kk", 0, entries)
	if err != nil {
		t.Fatal(err)
	}
	// Same backing array → cache returned the cached slice (regression: a
	// fresh marshal would allocate a new slice with a different header).
	if &a[0] != &b[0] {
		t.Fatal("R214-PERF-4 regression: marshalHistoryFrame did not return cached bytes; coalesce path broken")
	}
}
