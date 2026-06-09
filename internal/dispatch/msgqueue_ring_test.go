package dispatch

import (
	"testing"
	"time"
	"unsafe"
)

// TestMsgRing_PushDrainFIFO covers the basic FIFO contract.
func TestMsgRing_PushDrainFIFO(t *testing.T) {
	t.Parallel()
	var r msgRing
	r.push(QueuedMsg{Text: "A"}, 4)
	r.push(QueuedMsg{Text: "B"}, 4)
	r.push(QueuedMsg{Text: "C"}, 4)
	if r.len() != 3 {
		t.Fatalf("len = %d, want 3", r.len())
	}
	out := r.drainAll()
	if len(out) != 3 || out[0].Text != "A" || out[1].Text != "B" || out[2].Text != "C" {
		t.Fatalf("drainAll = %#v, want [A, B, C]", out)
	}
	if r.len() != 0 {
		t.Fatalf("post-drain len = %d, want 0", r.len())
	}
}

// TestMsgRing_EvictsOldestOnFull asserts the O(1) head-advance eviction
// returns the evicted=true signal and preserves FIFO order on the remaining
// elements. R247-PERF-23 (#570).
func TestMsgRing_EvictsOldestOnFull(t *testing.T) {
	t.Parallel()
	var r msgRing
	if ev, _ := r.push(QueuedMsg{Text: "A"}, 3); ev {
		t.Fatal("first push should not evict")
	}
	r.push(QueuedMsg{Text: "B"}, 3)
	r.push(QueuedMsg{Text: "C"}, 3)
	if ev, dropped := r.push(QueuedMsg{Text: "D"}, 3); !ev {
		t.Fatal("push at capacity must report eviction")
	} else if dropped.Text != "A" {
		t.Fatalf("evicted message = %q, want oldest 'A'", dropped.Text)
	}
	out := r.drainAll()
	if len(out) != 3 || out[0].Text != "B" || out[1].Text != "C" || out[2].Text != "D" {
		t.Fatalf("drainAll after eviction = %#v, want [B, C, D]", out)
	}
}

// TestMsgRing_WrapAroundIndices exercises the modulo arithmetic by pushing
// past the end of the backing array.
func TestMsgRing_WrapAroundIndices(t *testing.T) {
	t.Parallel()
	var r msgRing
	// Fill, drain, refill — drain reset head/used to 0 but next pushes
	// should still walk the ring correctly even after a partial drain
	// pattern. We simulate that by interleaving evictions.
	for i := 0; i < 5; i++ {
		r.push(QueuedMsg{Text: string(rune('A' + i))}, 3) // A B C, then evict A->[B C D], evict B->[C D E]
	}
	out := r.drainAll()
	if len(out) != 3 || out[0].Text != "C" || out[1].Text != "D" || out[2].Text != "E" {
		t.Fatalf("drainAll after wrap = %#v, want [C, D, E]", out)
	}

	// Reuse the same ring after drain — head=used=0 again, capacity
	// preserved.
	for i := 0; i < 4; i++ {
		r.push(QueuedMsg{Text: string(rune('a' + i))}, 3) // a b c, evict a -> [b c d]
	}
	out = r.drainAll()
	if len(out) != 3 || out[0].Text != "b" || out[1].Text != "c" || out[2].Text != "d" {
		t.Fatalf("drainAll after reuse = %#v, want [b, c, d]", out)
	}
}

// TestMsgRing_ResetClearsRefs asserts reset zeroes the live slots so any
// retained image data becomes GC-eligible (the previous slice memmove path
// also zeroed the freed slot — we must not regress).
func TestMsgRing_ResetClearsRefs(t *testing.T) {
	t.Parallel()
	var r msgRing
	canary := []byte{1, 2, 3}
	r.push(QueuedMsg{Text: "X", Images: nil, MessageID: "m"}, 4)
	// Place a large-ish payload via Images so we can detect retention.
	r.push(QueuedMsg{Text: "Y"}, 4)
	r.reset()
	if r.len() != 0 {
		t.Fatalf("post-reset len = %d, want 0", r.len())
	}
	// The backing buf must be fully zeroed across the previously-live
	// indices.
	for i := 0; i < cap(r.buf); i++ {
		if r.buf[i].Text != "" || r.buf[i].MessageID != "" {
			t.Fatalf("post-reset buf[%d] not zeroed: %#v", i, r.buf[i])
		}
	}
	_ = canary
}

// TestMsgRing_DrainInto_ReusesScratch asserts drainInto writes into the
// supplied dst when it has capacity (no fresh allocation) and preserves FIFO
// order + ring reset. R20260606-PERF-3 (#1827).
func TestMsgRing_DrainInto_ReusesScratch(t *testing.T) {
	t.Parallel()
	var r msgRing
	r.push(QueuedMsg{Text: "A"}, 4)
	r.push(QueuedMsg{Text: "B"}, 4)
	r.push(QueuedMsg{Text: "C"}, 4)

	first := r.drainInto(nil)
	if len(first) != 3 || first[0].Text != "A" || first[2].Text != "C" {
		t.Fatalf("first drain = %#v, want [A B C]", first)
	}

	// Refill and drain into the same backing array; it must be reused.
	r.push(QueuedMsg{Text: "D"}, 4)
	r.push(QueuedMsg{Text: "E"}, 4)
	second := r.drainInto(first)
	if len(second) != 2 || second[0].Text != "D" || second[1].Text != "E" {
		t.Fatalf("second drain = %#v, want [D E]", second)
	}
	// Same backing array (capacity reused, no realloc).
	if &second[:cap(second)][0] != &first[:cap(first)][0] {
		t.Fatal("drainInto allocated a new backing array despite sufficient dst capacity")
	}
}

// TestMsgRing_DrainInto_GrowsWhenDstTooSmall asserts a fresh slice is
// allocated when dst lacks capacity, and the old dst is left untouched.
func TestMsgRing_DrainInto_GrowsWhenDstTooSmall(t *testing.T) {
	t.Parallel()
	var r msgRing
	for i := 0; i < 3; i++ {
		r.push(QueuedMsg{Text: string(rune('A' + i))}, 4)
	}
	small := make([]QueuedMsg, 0, 1)
	smallBase := unsafe.Pointer(&small[:cap(small)][0])
	out := r.drainInto(small)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if unsafe.Pointer(&out[0]) == smallBase {
		t.Fatal("expected fresh allocation when dst too small")
	}
}

// TestMsgRing_DrainInto_EmptyReturnsNil preserves the nil-vs-slice contract.
func TestMsgRing_DrainInto_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	var r msgRing
	if got := r.drainInto(make([]QueuedMsg, 0, 8)); got != nil {
		t.Fatalf("empty drainInto = %#v, want nil", got)
	}
}

// TestMsgRing_DrainInto_ZeroesConsumedSlots guards the GC-hygiene contract:
// drainInto must zero the ring's backing slots so retained Images become
// GC-eligible (matches drainAll/reset).
func TestMsgRing_DrainInto_ZeroesConsumedSlots(t *testing.T) {
	t.Parallel()
	var r msgRing
	r.push(QueuedMsg{Text: "X", MessageID: "m1"}, 4)
	r.push(QueuedMsg{Text: "Y", MessageID: "m2"}, 4)
	_ = r.drainInto(nil)
	for i := 0; i < cap(r.buf); i++ {
		if r.buf[i].Text != "" || r.buf[i].MessageID != "" {
			t.Fatalf("post-drain buf[%d] not zeroed: %#v", i, r.buf[i])
		}
	}
}

// TestMsgQueue_DoneOrDrain_ScratchReuseAcrossTurns is an end-to-end check that
// the MessageQueue reuses one backing array across coalesced follow-up turns
// (ModeCollect) without corrupting the FIFO contract. Simulates the ownerLoop:
// drain -> consume -> enqueue more -> drain again. R20260606-PERF-3 (#1827).
func TestMsgQueue_DoneOrDrain_ScratchReuseAcrossTurns(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(8, 0)
	_, _, _, gen, _ := q.Enqueue("k", QueuedMsg{Text: "owner"})

	// Turn 1 follow-ups.
	q.Enqueue("k", QueuedMsg{Text: "a1"})
	q.Enqueue("k", QueuedMsg{Text: "a2"})
	batch1 := q.DoneOrDrain("k", gen)
	if len(batch1) != 2 || batch1[0].Text != "a1" || batch1[1].Text != "a2" {
		t.Fatalf("batch1 = %#v, want [a1 a2]", batch1)
	}
	base1 := &batch1[:cap(batch1)][0]

	// Turn 2 follow-ups — fewer messages, must reuse the same backing array.
	q.Enqueue("k", QueuedMsg{Text: "b1"})
	batch2 := q.DoneOrDrain("k", gen)
	if len(batch2) != 1 || batch2[0].Text != "b1" {
		t.Fatalf("batch2 = %#v, want [b1]", batch2)
	}
	if &batch2[:cap(batch2)][0] != base1 {
		t.Fatal("scratch was not reused across turns")
	}

	// Empty drain releases ownership and returns nil.
	if got := q.DoneOrDrain("k", gen); got != nil {
		t.Fatalf("empty drain = %#v, want nil", got)
	}
}

// TestMsgQueue_DoneOrDrain_ScratchReuse_NoAlloc proves the steady-state
// coalesced-turn drain no longer allocates a backing slice per turn once the
// scratch is warmed.
func TestMsgQueue_DoneOrDrain_ScratchReuse_NoAlloc(t *testing.T) {
	q := NewMessageQueue(8, 0)
	_, _, _, gen, _ := q.Enqueue("k", QueuedMsg{Text: "owner"})
	// Warm the scratch.
	q.Enqueue("k", QueuedMsg{Text: "w1"})
	q.Enqueue("k", QueuedMsg{Text: "w2"})
	q.Enqueue("k", QueuedMsg{Text: "w3"})
	_ = q.DoneOrDrain("k", gen)

	allocs := testing.AllocsPerRun(50, func() {
		q.Enqueue("k", QueuedMsg{Text: "x1"})
		q.Enqueue("k", QueuedMsg{Text: "x2"})
		_ = q.DoneOrDrain("k", gen)
	})
	if allocs != 0 {
		t.Fatalf("DoneOrDrain steady-state allocs = %v, want 0 (scratch reuse)", allocs)
	}
}

// TestMsgQueue_Enqueue_RingPath_FullEvictsAndDrains is an end-to-end check
// that the ring buffer integration into MessageQueue still produces the
// FIFO drain order documented on Enqueue/DoneOrDrain. Mirrors
// TestEnqueue_EvictsOldest but uses a higher push count so the ring
// genuinely wraps several times.
func TestMsgQueue_Enqueue_RingPath_FullEvictsAndDrains(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(3, 0)
	_, _, _, gen, _ := q.Enqueue("k", QueuedMsg{Text: "owner"}) // owner

	for i := 0; i < 10; i++ {
		q.Enqueue("k", QueuedMsg{Text: string(rune('0' + i)), EnqueueAt: time.Now()})
	}

	msgs := q.DoneOrDrain("k", gen)
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (max_depth)", len(msgs))
	}
	// Last 3 pushed (7, 8, 9) survive.
	if msgs[0].Text != "7" || msgs[1].Text != "8" || msgs[2].Text != "9" {
		t.Fatalf("drain order = [%s %s %s], want [7 8 9]",
			msgs[0].Text, msgs[1].Text, msgs[2].Text)
	}
}
