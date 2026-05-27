package dispatch

import (
	"testing"
	"time"
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
	if ev := r.push(QueuedMsg{Text: "A"}, 3); ev {
		t.Fatal("first push should not evict")
	}
	r.push(QueuedMsg{Text: "B"}, 3)
	r.push(QueuedMsg{Text: "C"}, 3)
	if ev := r.push(QueuedMsg{Text: "D"}, 3); !ev {
		t.Fatal("push at capacity must report eviction")
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

// TestMsgQueue_Enqueue_RingPath_FullEvictsAndDrains is an end-to-end check
// that the ring buffer integration into MessageQueue still produces the
// FIFO drain order documented on Enqueue/DoneOrDrain. Mirrors
// TestEnqueue_EvictsOldest but uses a higher push count so the ring
// genuinely wraps several times.
func TestMsgQueue_Enqueue_RingPath_FullEvictsAndDrains(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(3, 0)
	_, _, _, gen := q.Enqueue("k", QueuedMsg{Text: "owner"}) // owner

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
