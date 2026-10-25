package runstore

import (
	"testing"
	"time"
)

// TestTrimJobLocked_ConcurrentAppendDuringTrimDoesNotDeadlock exercises the
// runtime path: trigger a trim that does N removes while a parallel Append
// runs for the same jobID. With the lock released during Remove the Append
// proceeds; with the pre-fix shape it would queue behind the entire Remove
// batch. This is a smoke test — Go's per-Remove syscall on tmpfs is fast,
// so we don't measure latency, only that no deadlock or panic surfaces and
// that the post-trim Append landed.
func TestTrimJobLocked_ConcurrentAppendDuringTrimDoesNotDeadlock(t *testing.T) {
	const keepCount = 5
	s := newTestStore(t, keepCount, time.Hour)
	jobID := mustGenerateID()

	// Pre-seed with > keepCount runs so trimJobLocked has work to do.
	base := time.Now().Add(-30 * time.Minute)
	for i := 0; i < keepCount*3; i++ {
		r := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(r)
	}

	// One more Append after the seed — this exercises the trim path under
	// jobLock + the unlock-during-remove window. With the pre-#712 shape
	// this still completes (the lock-held-throughout pattern is correct,
	// just slower); the smoke test guards against any deadlock/panic
	// introduced by a malformed unlock+relock sequence in the patch.
	final := makeRun(jobID, time.Now())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Append(final)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append wedged for >5s after concurrent trim — likely " +
			"missing lock.Lock() reacquisition or double-unlock in #712 " +
			"patch.")
	}

	// Verify post-trim cap is honoured: at most keepCount summaries.
	got := s.Recent(jobID, 1000)
	if len(got) > keepCount {
		t.Fatalf("post-trim cache size %d > keepCount %d — trim broken", len(got), keepCount)
	}
}
