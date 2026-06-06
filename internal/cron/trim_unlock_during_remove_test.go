package cron

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestTrimJobLocked_ReleasesLockDuringRemoveBatch pins R246-GO-20 (#712):
// the os.Remove batch in trimJobLocked must run with jobLock RELEASED so a
// concurrent Append for the same jobID does not queue behind 200×Remove
// syscalls on a slow FUSE/NFS mount. The fix collects toRemove[] under
// jobLock, releases the lock for the syscall batch, then reacquires before
// cacheTrimAfterDisk.
//
// Structural pin: trimJobLocked source must contain the unlock-around-remove
// pattern (lock.Unlock() ... os.Remove(...) with reacquisition guaranteed).
// A future "simplification" that puts os.Remove back inside the held window
// (the pre-#712 shape) would erase the perf win and surface as a regression
// here rather than as latent jobLock contention in production.
//
// R20260527-GO-9 (#1271): the reacquisition is now scheduled via
// `defer lock.Lock()` inside the unlock closure so a panic from os.Remove
// (FUSE quirks, syscall traps) cannot leave the lock unlocked across the
// outer trimJobUnderLock's defer Unlock. This test accepts either the
// pre-#1271 bare reacquisition (lock.Unlock() ... os.Remove ... lock.Lock())
// or the post-#1271 defer-reacquire shape (lock.Unlock(); defer lock.Lock();
// ... os.Remove ...) — both preserve the lock-released-during-Remove
// contract that motivated #712.
func TestTrimJobLocked_ReleasesLockDuringRemoveBatch(t *testing.T) {
	src, err := os.ReadFile("runstore_trim.go")
	if err != nil {
		t.Fatalf("read runstore_trim.go: %v", err)
	}
	body := string(src)

	const fnMarker = "func (s *runStore) trimJobLocked("
	idx := strings.Index(body, fnMarker)
	if idx < 0 {
		t.Fatalf("trimJobLocked function not found in runstore_trim.go")
	}
	// Restrict to the function body up to the next top-level "func ".
	rest := body[idx:]
	if next := strings.Index(rest[len(fnMarker):], "\nfunc "); next >= 0 {
		rest = rest[:len(fnMarker)+next]
	}

	// 1. Must release jobLock before the os.Remove loop.
	idxUnlock := strings.Index(rest, "lock.Unlock()")
	if idxUnlock < 0 {
		t.Fatal("trimJobLocked must release jobLock during the os.Remove " +
			"batch (R246-GO-20 / #712). Without `lock.Unlock()` before the " +
			"remove loop, concurrent Append for the same jobID queues " +
			"behind N×Remove syscalls — pre-#712 regression.")
	}

	// 2. os.Remove call must appear AFTER the Unlock.
	idxRemove := strings.Index(rest[idxUnlock:], "os.Remove(")
	if idxRemove < 0 {
		t.Fatal("trimJobLocked: expected os.Remove call after lock.Unlock() " +
			"so the syscall batch runs without holding jobLock (#712).")
	}

	// 3. Must reacquire jobLock before returning so cacheTrimAfterDisk
	// stays serialised against concurrent cacheHeadPush. The reacquisition
	// can either be bare (lock.Lock() after the loop) or scheduled via
	// `defer lock.Lock()` BEFORE the loop (panic-safe shape from #1271).
	// Both are accepted; only "no reacquisition at all" should fail here.
	betweenUnlockAndRemove := rest[idxUnlock : idxUnlock+idxRemove]
	tail := rest[idxUnlock+idxRemove:]
	if !strings.Contains(tail, "lock.Lock()") &&
		!strings.Contains(betweenUnlockAndRemove, "defer lock.Lock()") {
		t.Fatal("trimJobLocked: expected lock.Lock() reacquisition after " +
			"the os.Remove batch (or `defer lock.Lock()` scheduled before " +
			"the batch for panic safety, #1271) so cacheTrimAfterDisk runs " +
			"under jobLock (matches the pre-#712 lock-order contract for " +
			"cacheHeadPush serialisation).")
	}

	// 4. cacheTrimAfterDisk must be the last cache operation, called after
	// reacquisition — pin its presence so the trim+cache reconciliation
	// stays atomic w.r.t. concurrent Append.
	if !strings.Contains(rest, "s.cacheTrimAfterDisk(jobID, cutoff)") {
		t.Fatal("trimJobLocked: expected cacheTrimAfterDisk(jobID, cutoff) " +
			"after the remove batch so the cache reconciles to the same " +
			"on-disk state.")
	}
}

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
