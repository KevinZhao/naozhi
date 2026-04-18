package dispatch

import (
	"sync"
	"testing"
	"time"
)

func TestEnqueue_FirstMessageBecomesOwner(t *testing.T) {
	q := NewMessageQueue(10, 500*time.Millisecond)
	isOwner, enqueued, gen := q.Enqueue("k1", QueuedMsg{Text: "hello"})
	if !isOwner {
		t.Fatal("first message should become owner")
	}
	if enqueued {
		t.Fatal("owner message should not be enqueued")
	}
	_ = gen
}

func TestEnqueue_SubsequentMessagesEnqueued(t *testing.T) {
	q := NewMessageQueue(10, 500*time.Millisecond)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	isOwner, enqueued, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if isOwner {
		t.Fatal("second message should not become owner")
	}
	if !enqueued {
		t.Fatal("second message should be enqueued")
	}

	if d := q.Depth("k1"); d != 1 {
		t.Fatalf("depth = %d, want 1", d)
	}
}

func TestEnqueue_MaxDepthZero_Drops(t *testing.T) {
	q := NewMessageQueue(0, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	isOwner, enqueued, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if isOwner || enqueued {
		t.Fatalf("maxDepth=0 should drop: isOwner=%v, enqueued=%v", isOwner, enqueued)
	}
}

func TestEnqueue_EvictsOldest(t *testing.T) {
	q := NewMessageQueue(2, 0)
	_, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	q.Enqueue("k1", QueuedMsg{Text: "B"})
	q.Enqueue("k1", QueuedMsg{Text: "C"})
	q.Enqueue("k1", QueuedMsg{Text: "D"}) // evicts B

	msgs := q.DoneOrDrain("k1", gen)
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Text != "C" || msgs[1].Text != "D" {
		t.Fatalf("want [C, D], got [%s, %s]", msgs[0].Text, msgs[1].Text)
	}
}

func TestDoneOrDrain_EmptyReleasesOwnership(t *testing.T) {
	q := NewMessageQueue(10, 0)
	_, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	msgs := q.DoneOrDrain("k1", gen)
	if msgs != nil {
		t.Fatalf("expected nil, got %d msgs", len(msgs))
	}

	// Ownership released — next enqueue should become owner.
	isOwner, _, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if !isOwner {
		t.Fatal("should become owner after release")
	}
}

func TestDoneOrDrain_NonEmptyKeepsOwnership(t *testing.T) {
	q := NewMessageQueue(10, 0)
	_, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner
	q.Enqueue("k1", QueuedMsg{Text: "B"})
	q.Enqueue("k1", QueuedMsg{Text: "C"})

	msgs := q.DoneOrDrain("k1", gen)
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}

	// Ownership still held — new enqueue should not become owner.
	isOwner, enqueued, _ := q.Enqueue("k1", QueuedMsg{Text: "D"})
	if isOwner {
		t.Fatal("should not become owner while still held")
	}
	if !enqueued {
		t.Fatal("should be enqueued")
	}
}

func TestDiscard_ClearsQueueAndReleasesOwnership(t *testing.T) {
	q := NewMessageQueue(10, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner
	q.Enqueue("k1", QueuedMsg{Text: "B"})

	q.Discard("k1")

	if d := q.Depth("k1"); d != 0 {
		t.Fatalf("depth = %d after discard", d)
	}

	// Next enqueue becomes owner.
	isOwner, _, _ := q.Enqueue("k1", QueuedMsg{Text: "C"})
	if !isOwner {
		t.Fatal("should become owner after discard")
	}
}

func TestDiscard_InvalidatesStaleOwner(t *testing.T) {
	q := NewMessageQueue(10, 0)
	_, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // gen=0
	q.Enqueue("k1", QueuedMsg{Text: "B"})

	// Simulate /new: discard bumps generation.
	q.Discard("k1")

	// New owner starts with new generation.
	_, _, gen2 := q.Enqueue("k1", QueuedMsg{Text: "C"})
	q.Enqueue("k1", QueuedMsg{Text: "D"})

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
	q := NewMessageQueue(10, 0)

	if !q.ShouldNotify("k1") {
		t.Fatal("first call should return true")
	}
	if q.ShouldNotify("k1") {
		t.Fatal("immediate second call should return false")
	}
}

func TestIsolation_DifferentKeys(t *testing.T) {
	q := NewMessageQueue(10, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // k1 owner
	q.Enqueue("k2", QueuedMsg{Text: "B"}) // k2 owner — independent

	isOwner, _, _ := q.Enqueue("k1", QueuedMsg{Text: "C"})
	if isOwner {
		t.Fatal("k1 is busy, should not become owner")
	}

	isOwner, _, _ = q.Enqueue("k2", QueuedMsg{Text: "D"})
	if isOwner {
		t.Fatal("k2 is busy, should not become owner")
	}
}

func TestSessionGuardCompat_TryAcquireRelease(t *testing.T) {
	q := NewMessageQueue(10, 0)

	if !q.TryAcquire("k1") {
		t.Fatal("TryAcquire should succeed on idle key")
	}
	if q.TryAcquire("k1") {
		t.Fatal("TryAcquire should fail on busy key")
	}

	q.Release("k1")

	if !q.TryAcquire("k1") {
		t.Fatal("TryAcquire should succeed after Release")
	}
}

func TestLastNotify_CleanedOnDrain(t *testing.T) {
	q := NewMessageQueue(10, 0)
	_, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"})

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
	q := NewMessageQueue(10, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"})
	q.ShouldNotify("k1")

	q.Discard("k1")

	if !q.ShouldNotify("k1") {
		t.Fatal("lastNotify should be cleaned after Discard")
	}
}

// TestConcurrent_EnqueueDrain verifies no races under concurrent access.
func TestConcurrent_EnqueueDrain(t *testing.T) {
	q := NewMessageQueue(50, 0)
	const goroutines = 20
	const msgsPerGoroutine = 100

	var wg sync.WaitGroup

	// Spawn goroutines that enqueue messages.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < msgsPerGoroutine; j++ {
				q.Enqueue("shared", QueuedMsg{Text: "msg"})
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
				q.Depth("shared")
			}
		}()
	}

	wg.Wait()
}
