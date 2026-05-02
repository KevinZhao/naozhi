package dispatch

import (
	"sync"
	"testing"
	"time"
)

func TestEnqueue_FirstMessageBecomesOwner(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(10, 500*time.Millisecond)
	isOwner, enqueued, _, gen := q.Enqueue("k1", QueuedMsg{Text: "hello"})
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
	q := NewMessageQueue(10, 500*time.Millisecond)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	isOwner, enqueued, _, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
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
	t.Parallel()
	q := NewMessageQueue(0, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	isOwner, enqueued, _, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if isOwner || enqueued {
		t.Fatalf("maxDepth=0 should drop: isOwner=%v, enqueued=%v", isOwner, enqueued)
	}
}

func TestEnqueue_EvictsOldest(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(2, 0)
	_, _, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

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
	t.Parallel()
	q := NewMessageQueue(10, 0)
	_, _, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	msgs := q.DoneOrDrain("k1", gen)
	if msgs != nil {
		t.Fatalf("expected nil, got %d msgs", len(msgs))
	}

	// Ownership released — next enqueue should become owner.
	isOwner, _, _, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if !isOwner {
		t.Fatal("should become owner after release")
	}
}

func TestDoneOrDrain_NonEmptyKeepsOwnership(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(10, 0)
	_, _, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner
	q.Enqueue("k1", QueuedMsg{Text: "B"})
	q.Enqueue("k1", QueuedMsg{Text: "C"})

	msgs := q.DoneOrDrain("k1", gen)
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}

	// Ownership still held — new enqueue should not become owner.
	isOwner, enqueued, _, _ := q.Enqueue("k1", QueuedMsg{Text: "D"})
	if isOwner {
		t.Fatal("should not become owner while still held")
	}
	if !enqueued {
		t.Fatal("should be enqueued")
	}
}

func TestDiscard_ClearsQueueAndReleasesOwnership(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(10, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner
	q.Enqueue("k1", QueuedMsg{Text: "B"})

	q.Discard("k1")

	if d := q.Depth("k1"); d != 0 {
		t.Fatalf("depth = %d after discard", d)
	}

	// Next enqueue becomes owner.
	isOwner, _, _, _ := q.Enqueue("k1", QueuedMsg{Text: "C"})
	if !isOwner {
		t.Fatal("should become owner after discard")
	}
}

func TestDiscard_InvalidatesStaleOwner(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(10, 0)
	_, _, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // gen=0
	q.Enqueue("k1", QueuedMsg{Text: "B"})

	// Simulate /new: discard bumps generation.
	q.Discard("k1")

	// New owner starts with new generation.
	_, _, _, gen2 := q.Enqueue("k1", QueuedMsg{Text: "C"})
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
	t.Parallel()
	q := NewMessageQueue(10, 0)

	if !q.ShouldNotify("k1") {
		t.Fatal("first call should return true")
	}
	if q.ShouldNotify("k1") {
		t.Fatal("immediate second call should return false")
	}
}

func TestIsolation_DifferentKeys(t *testing.T) {
	t.Parallel()
	q := NewMessageQueue(10, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // k1 owner
	q.Enqueue("k2", QueuedMsg{Text: "B"}) // k2 owner — independent

	isOwner, _, _, _ := q.Enqueue("k1", QueuedMsg{Text: "C"})
	if isOwner {
		t.Fatal("k1 is busy, should not become owner")
	}

	isOwner, _, _, _ = q.Enqueue("k2", QueuedMsg{Text: "D"})
	if isOwner {
		t.Fatal("k2 is busy, should not become owner")
	}
}

func TestSessionGuardCompat_TryAcquireRelease(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	q := NewMessageQueue(10, 0)
	_, _, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A"})

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
	q := NewMessageQueue(10, 0)
	q.Enqueue("k1", QueuedMsg{Text: "A"})
	q.ShouldNotify("k1")

	q.Discard("k1")

	if !q.ShouldNotify("k1") {
		t.Fatal("lastNotify should be cleaned after Discard")
	}
}

func TestParseQueueMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want QueueMode
	}{
		{"", ModeCollect},
		{"collect", ModeCollect},
		{"COLLECT", ModeCollect},
		{"interrupt", ModeInterrupt},
		{"  Interrupt ", ModeInterrupt},
		{"unknown", ModeCollect},
	}
	for _, c := range cases {
		if got := ParseQueueMode(c.in); got != c.want {
			t.Errorf("ParseQueueMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEnqueue_CollectMode_NoInterruptSignal(t *testing.T) {
	t.Parallel()
	q := NewMessageQueueWithMode(10, 0, ModeCollect)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	_, enqueued, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if !enqueued {
		t.Fatal("B should be enqueued")
	}
	if shouldInterrupt {
		t.Fatal("Collect mode must never set shouldInterrupt")
	}
}

func TestEnqueue_InterruptMode_FirstFollowupSetsSignal(t *testing.T) {
	t.Parallel()
	q := NewMessageQueueWithMode(10, 0, ModeInterrupt)
	_, _, shouldInterruptOwner, gen := q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner
	if shouldInterruptOwner {
		t.Fatal("owner path must not request interrupt (nothing to interrupt yet)")
	}

	_, enqueued, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
	if !enqueued {
		t.Fatal("B should be enqueued")
	}
	if !shouldInterrupt {
		t.Fatal("first follow-up in Interrupt mode must set shouldInterrupt")
	}

	// Second queued message on the same running turn must NOT re-signal.
	_, _, shouldInterrupt2, _ := q.Enqueue("k1", QueuedMsg{Text: "C"})
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
	_, _, shouldInterrupt3, _ := q.Enqueue("k1", QueuedMsg{Text: "D"})
	if !shouldInterrupt3 {
		t.Fatal("after drain, next turn's first follow-up must interrupt again")
	}
}

func TestEnqueue_InterruptMode_QueueDisabledNoSignal(t *testing.T) {
	t.Parallel()
	q := NewMessageQueueWithMode(0, 0, ModeInterrupt)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner

	_, enqueued, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "B"})
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
	q := NewMessageQueueWithMode(10, 0, ModeInterrupt)

	// Turn 1: owner + interrupting follow-up.
	_, _, _, gen := q.Enqueue("k1", QueuedMsg{Text: "A1"})
	if _, _, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "B1"}); !shouldInterrupt {
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
	_, _, _, gen2 := q.Enqueue("k1", QueuedMsg{Text: "A2"})
	if gen2 == gen {
		// Not strictly required (release path does not bump gen), but document
		// the assumption: same sessionQueue key, ownership cycled.
		_ = gen2
	}
	if _, _, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "B2"}); !shouldInterrupt {
		t.Fatal("turn 2 follow-up after ownership release must request interrupt")
	}
}

// P1-2 part two: Discard must also reset interruptRequested so /new followed
// by a fresh turn does not silently suppress the first interrupt.
func TestEnqueue_InterruptMode_Discard_ResetsInterruptFlag(t *testing.T) {
	t.Parallel()
	q := NewMessageQueueWithMode(10, 0, ModeInterrupt)
	q.Enqueue("k1", QueuedMsg{Text: "A"}) // owner
	if _, _, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "B"}); !shouldInterrupt {
		t.Fatal("first follow-up must interrupt")
	}

	// /new — discard everything.
	q.Discard("k1")

	// New owner.
	q.Enqueue("k1", QueuedMsg{Text: "C"})
	if _, _, shouldInterrupt, _ := q.Enqueue("k1", QueuedMsg{Text: "D"}); !shouldInterrupt {
		t.Fatal("after Discard, next turn's first follow-up must interrupt")
	}
}

// TestConcurrent_EnqueueDrain verifies no races under concurrent access.
func TestConcurrent_EnqueueDrain(t *testing.T) {
	t.Parallel()
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
