package turn

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnqueue_FirstMessageBecomesOwner(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 500*time.Millisecond)
	isOwner, enqueued, _, gen, _ := q.Enqueue("k1", Msg{Text: "hello"})
	if !isOwner {
		t.Fatal("first message should become owner")
	}
	if enqueued {
		t.Fatal("owner message should not be enqueued")
	}
	_ = gen
}

func TestEnqueue_SubsequentMessagesEnqueued(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 500*time.Millisecond)
	q.Enqueue("k1", Msg{Text: "A"}) // owner

	isOwner, enqueued, _, _, _ := q.Enqueue("k1", Msg{Text: "B"})
	if isOwner {
		t.Fatal("second message should not become owner")
	}
	if !enqueued {
		t.Fatal("second message should be enqueued")
	}

	if d := q.depth("k1"); d != 1 {
		t.Fatalf("depth = %d, want 1", d)
	}
}

func TestEnqueue_MaxDepthZero_Drops(t *testing.T) {
	t.Parallel()
	q := NewQueue(0, 0)
	q.Enqueue("k1", Msg{Text: "A"}) // owner

	isOwner, enqueued, _, _, _ := q.Enqueue("k1", Msg{Text: "B"})
	if isOwner || enqueued {
		t.Fatalf("maxDepth=0 should drop: isOwner=%v, enqueued=%v", isOwner, enqueued)
	}
}

func TestEnqueue_EvictsOldest(t *testing.T) {
	t.Parallel()
	q := NewQueue(2, 0)
	_, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "A"}) // owner

	q.Enqueue("k1", Msg{Text: "B", MessageID: "mB"})
	q.Enqueue("k1", Msg{Text: "C", MessageID: "mC"})
	// Queue full (depth 2): D evicts the oldest (B). #1945: Enqueue must
	// surface B's MessageID so the caller can clear B's dangling reaction.
	_, _, _, _, evictedID := q.Enqueue("k1", Msg{Text: "D", MessageID: "mD"})
	if evictedID != "mB" {
		t.Fatalf("evictedID = %q, want oldest 'mB' so its queued reaction can be cleared (#1945)", evictedID)
	}

	msgs := q.DoneOrDrain("k1", gen)
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Text != "C" || msgs[1].Text != "D" {
		t.Fatalf("want [C, D], got [%s, %s]", msgs[0].Text, msgs[1].Text)
	}
}

// TestEnqueue_NoEvictionReturnsEmptyID pins that a non-full enqueue reports
// no eviction (empty evictedID) so the caller never spuriously tries to clear
// a reaction. #1945.
func TestEnqueue_NoEvictionReturnsEmptyID(t *testing.T) {
	t.Parallel()
	q := NewQueue(4, 0)
	q.Enqueue("k1", Msg{Text: "A", MessageID: "mA"}) // owner
	_, _, _, _, evictedID := q.Enqueue("k1", Msg{Text: "B", MessageID: "mB"})
	if evictedID != "" {
		t.Fatalf("evictedID = %q, want empty when queue not full", evictedID)
	}
}

func TestDoneOrDrain_EmptyReleasesOwnership(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	_, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "A"}) // owner

	msgs := q.DoneOrDrain("k1", gen)
	if msgs != nil {
		t.Fatalf("expected nil, got %d msgs", len(msgs))
	}

	// Ownership released — next enqueue should become owner.
	isOwner, _, _, _, _ := q.Enqueue("k1", Msg{Text: "B"})
	if !isOwner {
		t.Fatal("should become owner after release")
	}
}

func TestDoneOrDrain_NonEmptyKeepsOwnership(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	_, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "A"}) // owner
	q.Enqueue("k1", Msg{Text: "B"})
	q.Enqueue("k1", Msg{Text: "C"})

	msgs := q.DoneOrDrain("k1", gen)
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}

	// Ownership still held — new enqueue should not become owner.
	isOwner, enqueued, _, _, _ := q.Enqueue("k1", Msg{Text: "D"})
	if isOwner {
		t.Fatal("should not become owner while still held")
	}
	if !enqueued {
		t.Fatal("should be enqueued")
	}
}

func TestDiscard_ClearsQueueAndReleasesOwnership(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	q.Enqueue("k1", Msg{Text: "A"}) // owner
	q.Enqueue("k1", Msg{Text: "B"})

	q.Discard("k1")

	if d := q.depth("k1"); d != 0 {
		t.Fatalf("depth = %d after discard", d)
	}

	// Next enqueue becomes owner.
	isOwner, _, _, _, _ := q.Enqueue("k1", Msg{Text: "C"})
	if !isOwner {
		t.Fatal("should become owner after discard")
	}
}

func TestDiscard_InvalidatesStaleOwner(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	_, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "A"}) // gen=0
	q.Enqueue("k1", Msg{Text: "B"})

	// Simulate /new: discard bumps generation.
	q.Discard("k1")

	// New owner starts with new generation.
	_, _, _, gen2, _ := q.Enqueue("k1", Msg{Text: "C"})
	q.Enqueue("k1", Msg{Text: "D"})

	// Stale owner tries DoneOrDrain with old gen — should get nil.
	msgs := q.DoneOrDrain("k1", gen)
	if msgs != nil {
		t.Fatalf("stale owner should get nil, got %d msgs", len(msgs))
	}

	// New owner drains successfully with correct gen.
	msgs = q.DoneOrDrain("k1", gen2)
	if len(msgs) != 1 || msgs[0].Text != "D" {
		t.Fatalf("new owner should drain [D], got %v", msgs)
	}
}

func TestShouldNotify_RateLimits(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)

	if !q.ShouldNotify("k1") {
		t.Fatal("first call should return true")
	}
	if q.ShouldNotify("k1") {
		t.Fatal("immediate second call should return false")
	}
}

// TestShouldNotify_ConcurrentRLockFastPath exercises the R20260528-PERF-19
// (#1358) fast-path: many goroutines hammering ShouldNotify on the same
// already-cooled-down key. -race covers the RLock→RUnlock→Lock window
// against concurrent slow-path mutations; the test then verifies that
// after the storm exactly one notify can have fired (the cooldown is
// active for everyone after the first observer).
func TestShouldNotify_ConcurrentRLockFastPath(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)

	// Prime the queue branch: Enqueue makes a sessionQueue in q.queues,
	// then ShouldNotify publishes lastNotifyNs so subsequent calls take
	// the cooldown-active fast path.
	q.Enqueue("k", Msg{Text: "owner"})
	if !q.ShouldNotify("k") {
		t.Fatal("first ShouldNotify should fire (cooldown empty)")
	}

	// Storm: 64 goroutines × 100 calls — every call should see the
	// cooldown active and return false via the RLock fast path. The race
	// detector catches any read-write order violation if an internal
	// mutation slips out from under the read lock.
	const goroutines = 64
	const perGo = 100
	var fired atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGo; j++ {
				if q.ShouldNotify("k") {
					fired.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := fired.Load(); got != 0 {
		t.Fatalf("fired = %d during cooldown window, want 0 (fast-path must observe cooldown)", got)
	}
}

func TestIsolation_DifferentKeys(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	q.Enqueue("k1", Msg{Text: "A"}) // k1 owner
	q.Enqueue("k2", Msg{Text: "B"}) // k2 owner — independent

	isOwner, _, _, _, _ := q.Enqueue("k1", Msg{Text: "C"})
	if isOwner {
		t.Fatal("k1 is busy, should not become owner")
	}

	isOwner, _, _, _, _ = q.Enqueue("k2", Msg{Text: "D"})
	if isOwner {
		t.Fatal("k2 is busy, should not become owner")
	}
}

func TestLastNotify_CleanedOnDrain(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	_, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "A"})

	// Trigger a notify entry.
	q.ShouldNotify("k1")

	// Drain with empty queue releases.
	q.DoneOrDrain("k1", gen)

	// After cleanup, ShouldNotify should return true (entry was deleted).
	if !q.ShouldNotify("k1") {
		t.Fatal("lastNotify should be cleaned after DoneOrDrain release")
	}
}

func TestLastNotify_CleanedOnDiscard(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	q.Enqueue("k1", Msg{Text: "A"})
	q.ShouldNotify("k1")

	q.Discard("k1")

	if !q.ShouldNotify("k1") {
		t.Fatal("lastNotify should be cleaned after Discard")
	}
}

// TestShouldNotify_PoolRecyclesEvictedEntry verifies that the dropNotifyEntry
// pool (#1694) preserves correctness across LRU saturation: once the LRU is
// full, inserting new cold keys must evict the oldest, recycle its struct, and
// still maintain per-key cooldown for the surviving keys. A recycled entry
// must not carry over a stale key/ts.
func TestShouldNotify_PoolRecyclesEvictedEntry(t *testing.T) {
	t.Parallel()
	q := NewQueue(0, 0) // maxDepth<=0 forces the drop/LRU path

	// Saturate the LRU with dropNotifyMaxKeys distinct cold keys. Each first
	// call fires; immediate second call on the same key is cooled down.
	for i := 0; i < dropNotifyMaxKeys; i++ {
		k := fmt.Sprintf("k%d", i)
		if !q.ShouldNotify(k) {
			t.Fatalf("first ShouldNotify(%q) should fire", k)
		}
		if q.ShouldNotify(k) {
			t.Fatalf("second ShouldNotify(%q) should be cooled down", k)
		}
	}

	// LRU is now full. Insert one more cold key; this evicts the oldest (k0)
	// and must reuse its struct.
	overflow := fmt.Sprintf("k%d", dropNotifyMaxKeys)
	if !q.ShouldNotify(overflow) {
		t.Fatalf("overflow key %q should fire", overflow)
	}
	if q.ShouldNotify(overflow) {
		t.Fatalf("overflow key %q should be cooled down after first fire", overflow)
	}

	// The evicted key (k0) has no remaining entry, so it should fire again as
	// a fresh cold key — proving the recycled struct did not retain k0's ts.
	if !q.ShouldNotify("k0") {
		t.Fatal("evicted key k0 should fire again (entry was recycled, not stale)")
	}
}

// BenchmarkShouldNotify_ColdKeyChurn measures the steady-state cold-key insert
// path that #1694 optimizes. After the LRU saturates, each iteration evicts a
// tail entry and inserts a new one; with the pool this should allocate nothing.
func BenchmarkShouldNotify_ColdKeyChurn(b *testing.B) {
	q := NewQueue(0, 0)
	// Pre-saturate so every benchmarked call hits the evict+recycle path.
	for i := 0; i < dropNotifyMaxKeys; i++ {
		q.ShouldNotify(fmt.Sprintf("warm%d", i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.ShouldNotify(fmt.Sprintf("cold%d", i))
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want Mode
	}{
		{"", ModeCollect},
		{"collect", ModeCollect},
		{"COLLECT", ModeCollect},
		{"interrupt", ModeInterrupt},
		{"  Interrupt ", ModeInterrupt},
		{"unknown", ModeCollect},
	}
	for _, c := range cases {
		if got := ParseMode(c.in); got != c.want {
			t.Errorf("ParseMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEnqueue_CollectMode_NoInterruptSignal(t *testing.T) {
	t.Parallel()
	q := NewQueueWithMode(10, 0, ModeCollect)
	q.Enqueue("k1", Msg{Text: "A"}) // owner

	_, enqueued, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "B"})
	if !enqueued {
		t.Fatal("B should be enqueued")
	}
	if shouldInterrupt {
		t.Fatal("Collect mode must never set shouldInterrupt")
	}
}

func TestEnqueue_InterruptMode_FirstFollowupSetsSignal(t *testing.T) {
	t.Parallel()
	q := NewQueueWithMode(10, 0, ModeInterrupt)
	_, _, shouldInterruptOwner, gen, _ := q.Enqueue("k1", Msg{Text: "A"}) // owner
	if shouldInterruptOwner {
		t.Fatal("owner path must not request interrupt (nothing to interrupt yet)")
	}

	_, enqueued, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "B"})
	if !enqueued {
		t.Fatal("B should be enqueued")
	}
	if !shouldInterrupt {
		t.Fatal("first follow-up in Interrupt mode must set shouldInterrupt")
	}

	// Second queued message on the same running turn must NOT re-signal.
	_, _, shouldInterrupt2, _, _ := q.Enqueue("k1", Msg{Text: "C"})
	if shouldInterrupt2 {
		t.Fatal("second follow-up must not re-trigger interrupt")
	}

	// After owner drains the queued batch, the turn's interrupt state must
	// reset so the NEXT turn's first follow-up can interrupt again.
	drained := q.DoneOrDrain("k1", gen)
	if len(drained) != 2 {
		t.Fatalf("drained = %d, want 2", len(drained))
	}
	// Simulate next turn completing: owner still holds ownership, a new
	// follow-up arrives during the next in-flight turn.
	_, _, shouldInterrupt3, _, _ := q.Enqueue("k1", Msg{Text: "D"})
	if !shouldInterrupt3 {
		t.Fatal("after drain, next turn's first follow-up must interrupt again")
	}
}

func TestEnqueue_InterruptMode_QueueDisabledNoSignal(t *testing.T) {
	t.Parallel()
	q := NewQueueWithMode(0, 0, ModeInterrupt)
	q.Enqueue("k1", Msg{Text: "A"}) // owner

	_, enqueued, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "B"})
	if enqueued {
		t.Fatal("disabled queue must drop")
	}
	if shouldInterrupt {
		t.Fatal("dropped message must not emit interrupt signal (nothing to deliver after abort)")
	}
}

// Regression test for the P1-2 concern: releasing ownership when the queue
// drains empty must reset interruptRequested so a later Enqueue that re-owns
// the session (new turn) can again signal shouldInterrupt on its first
// follow-up. Without the explicit reset in DoneOrDrain, a refactor that
// reused the *sessionQueue instance instead of going through getOrCreate
// would silently suppress the interrupt.
func TestEnqueue_InterruptMode_ReleaseOwnership_ResetsInterruptFlag(t *testing.T) {
	t.Parallel()
	q := NewQueueWithMode(10, 0, ModeInterrupt)

	// Turn 1: owner + interrupting follow-up.
	_, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "A1"})
	if _, _, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "B1"}); !shouldInterrupt {
		t.Fatal("turn 1 follow-up must request interrupt")
	}

	// Owner drains batch (turn 1 completes with queued follow-up → interrupt path).
	if drained := q.DoneOrDrain("k1", gen); len(drained) != 1 {
		t.Fatalf("drained turn 1 = %d msgs, want 1", len(drained))
	}
	// Owner drains again; queue is now empty → ownership released.
	if drained := q.DoneOrDrain("k1", gen); drained != nil {
		t.Fatalf("drained on empty queue should return nil, got %d msgs", len(drained))
	}

	// Turn 2: new owner arrives (fresh session/chat activity). A follow-up
	// during turn 2 must again be able to trigger an interrupt, proving the
	// release path reset interruptRequested.
	_, _, _, gen2, _ := q.Enqueue("k1", Msg{Text: "A2"})
	if gen2 == gen {
		// Not strictly required (release path does not bump gen), but document
		// the assumption: same sessionQueue key, ownership cycled.
		_ = gen2
	}
	if _, _, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "B2"}); !shouldInterrupt {
		t.Fatal("turn 2 follow-up after ownership release must request interrupt")
	}
}

// P1-2 part two: Discard must also reset interruptRequested so /new followed
// by a fresh turn does not silently suppress the first interrupt.
func TestEnqueue_InterruptMode_Discard_ResetsInterruptFlag(t *testing.T) {
	t.Parallel()
	q := NewQueueWithMode(10, 0, ModeInterrupt)
	q.Enqueue("k1", Msg{Text: "A"}) // owner
	if _, _, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "B"}); !shouldInterrupt {
		t.Fatal("first follow-up must interrupt")
	}

	// /new — discard everything.
	q.Discard("k1")

	// New owner.
	q.Enqueue("k1", Msg{Text: "C"})
	if _, _, shouldInterrupt, _, _ := q.Enqueue("k1", Msg{Text: "D"}); !shouldInterrupt {
		t.Fatal("after Discard, next turn's first follow-up must interrupt")
	}
}

// TestQueue_Cleanup_RemovesMapEntry verifies Cleanup drops the entry Discard
// retains for gen-monotonicity, and the next Enqueue starts at gen=0.
func TestQueue_Cleanup_RemovesMapEntry(t *testing.T) {
	t.Parallel()
	q := NewQueue(10, 0)
	q.Enqueue("k1", Msg{Text: "A"})
	q.Enqueue("k2", Msg{Text: "A"})
	q.Discard("k1") // retains the map entry with bumped gen

	q.mu.Lock()
	before := len(q.queues)
	_, retained := q.queues["k1"]
	q.mu.Unlock()
	if !retained {
		t.Fatal("Discard should retain the map entry")
	}

	q.Cleanup("k1")
	q.Cleanup("never-seen") // no-op on unknown key

	q.mu.Lock()
	after := len(q.queues)
	_, present := q.queues["k1"]
	q.mu.Unlock()
	if present {
		t.Fatal("Cleanup should delete the map entry")
	}
	if got := before - after; got != 1 {
		t.Fatalf("len(queues) dropped by %d, want 1", got)
	}

	isOwner, _, _, gen, _ := q.Enqueue("k1", Msg{Text: "fresh"})
	if !isOwner || gen != 0 {
		t.Fatalf("post-Cleanup: isOwner=%v, gen=%d; want true, 0", isOwner, gen)
	}
}

// TestConcurrent_EnqueueDrain verifies no races under concurrent access.
func TestConcurrent_EnqueueDrain(t *testing.T) {
	t.Parallel()
	q := NewQueue(50, 0)
	const goroutines = 20
	const msgsPerGoroutine = 100

	var wg sync.WaitGroup

	// Spawn goroutines that enqueue messages.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < msgsPerGoroutine; j++ {
				q.Enqueue("shared", Msg{Text: "msg"})
			}
		}()
	}

	// Spawn goroutines that drain.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < msgsPerGoroutine; j++ {
				q.DoneOrDrain("shared", 0) // gen=0 matches initial
				q.depth("shared")
			}
		}()
	}

	wg.Wait()
}

// ---------------------------------------------------------------------------
// ShouldNotify dropNotifyTimes path
// ---------------------------------------------------------------------------
//
// These construct a Queue directly and never touch a Dispatcher.

func TestShouldNotify_DropPath(t *testing.T) {
	q := NewQueue(0, 0)
	if !q.ShouldNotify("k") {
		t.Fatal("first call should return true")
	}
	if q.ShouldNotify("k") {
		t.Fatal("immediate second call should be rate-limited")
	}
}

func TestShouldNotify_DropPath_Eviction(t *testing.T) {
	q := NewQueue(0, 0)
	for i := 0; i < dropNotifyMaxKeys; i++ {
		q.ShouldNotify(fmt.Sprintf("key-%d", i))
	}
	if !q.ShouldNotify("overflow-key") {
		t.Fatal("should notify after eviction at capacity")
	}
	q.mu.Lock()
	size := q.dropNotifyLRU.Len()
	idxSize := len(q.dropNotifyIndex)
	q.mu.Unlock()
	if size > dropNotifyMaxKeys {
		t.Errorf("dropNotifyLRU size = %d > cap %d", size, dropNotifyMaxKeys)
	}
	if idxSize != size {
		t.Errorf("dropNotifyIndex size %d != LRU size %d", idxSize, size)
	}
}

// TestShouldNotify_DropPath_BackPointerConsistent pins the R249-PERF-12 (#932)
// refactor invariant: dropNotifyIndex maps directly to *dropNotifyEntry and each
// entry's elem back-pointer must always reference the live list element, so the
// LRU stays in lock-step with the map across refresh and eviction.
func TestShouldNotify_DropPath_BackPointerConsistent(t *testing.T) {
	q := NewQueue(0, 0)
	q.ShouldNotify("a")
	q.ShouldNotify("b")

	q.mu.Lock()
	for key, entry := range q.dropNotifyIndex {
		if entry.elem == nil {
			t.Fatalf("entry %q has nil elem back-pointer", key)
		}
		if got := entry.elem.Value.(*dropNotifyEntry); got != entry {
			t.Fatalf("entry %q elem points at a different entry", key)
		}
		if entry.key != key {
			t.Fatalf("index key %q != entry.key %q", key, entry.key)
		}
	}
	q.mu.Unlock()

	// Fill to capacity then overflow; the eviction must drop exactly one key
	// and keep the map and list the same size (no dangling back-pointers).
	for i := 0; i < dropNotifyMaxKeys; i++ {
		q.ShouldNotify(fmt.Sprintf("fill-%d", i))
	}
	q.mu.Lock()
	if q.dropNotifyLRU.Len() != len(q.dropNotifyIndex) {
		t.Fatalf("LRU/index size mismatch after overflow: %d vs %d",
			q.dropNotifyLRU.Len(), len(q.dropNotifyIndex))
	}
	for e := q.dropNotifyLRU.Front(); e != nil; e = e.Next() {
		entry := e.Value.(*dropNotifyEntry)
		if q.dropNotifyIndex[entry.key] != entry {
			t.Fatalf("list entry %q missing/stale in index", entry.key)
		}
	}
	q.mu.Unlock()
}
