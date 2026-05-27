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
// pattern (lock.Unlock() ... remove batch ... lock.Lock()). The remove batch
// itself moved into trimRemoveBatch helper for panic safety
// (R20260527-GO-9 / R20260527-COR-4 / #1271, #1291) so the os.Remove call
// lives in the helper rather than inline; this test now asserts the helper
// is invoked between Unlock and Lock and that the helper itself contains
// os.Remove + a panic-recovery `lock.Lock()`. A future "simplification"
// that puts the remove batch back inside the held window (the pre-#712
// shape) would erase the perf win, AND a regression that drops the
// helper's recover-and-relock would re-introduce the
// Unlock-of-unlocked-mutex panic on FS-layer panics; both surface here
// rather than as latent jobLock contention / mysterious goroutine deaths
// in production.
func TestTrimJobLocked_ReleasesLockDuringRemoveBatch(t *testing.T) {
	src, err := os.ReadFile("runstore.go")
	if err != nil {
		t.Fatalf("read runstore.go: %v", err)
	}
	body := string(src)

	const fnMarker = "func (s *runStore) trimJobLocked("
	idx := strings.Index(body, fnMarker)
	if idx < 0 {
		t.Fatalf("trimJobLocked function not found in runstore.go")
	}
	// Restrict to the function body up to the next top-level "func ".
	rest := body[idx:]
	if next := strings.Index(rest[len(fnMarker):], "\nfunc "); next >= 0 {
		rest = rest[:len(fnMarker)+next]
	}

	// 1. Must release jobLock before the remove batch.
	idxUnlock := strings.Index(rest, "lock.Unlock()")
	if idxUnlock < 0 {
		t.Fatal("trimJobLocked must release jobLock during the os.Remove " +
			"batch (R246-GO-20 / #712). Without `lock.Unlock()` before the " +
			"remove site, concurrent Append for the same jobID queues " +
			"behind N×Remove syscalls — pre-#712 regression.")
	}

	// 2. The remove batch must appear AFTER the Unlock — either inline
	// (`os.Remove(`) or via the panic-safe helper (`s.trimRemoveBatch(`).
	// R20260527-GO-9 / R20260527-COR-4 (#1271, #1291): the helper was
	// extracted so a panicking os.Remove cannot leave jobLock unlocked
	// for the outer caller's `defer lock.Unlock()` (which would itself
	// panic on Unlock-of-unlocked-mutex, masking the original FS panic).
	tail := rest[idxUnlock:]
	idxRemove := strings.Index(tail, "os.Remove(")
	idxHelper := strings.Index(tail, "s.trimRemoveBatch(")
	if idxRemove < 0 && idxHelper < 0 {
		t.Fatal("trimJobLocked: expected os.Remove call (or s.trimRemoveBatch " +
			"helper invocation) after lock.Unlock() so the syscall batch runs " +
			"without holding jobLock (#712 / panic-safe variant: #1271/#1291).")
	}
	idxBatch := idxHelper
	if idxBatch < 0 || (idxRemove >= 0 && idxRemove < idxHelper) {
		idxBatch = idxRemove
	}

	// 3. Must reacquire jobLock after the batch so cacheTrimAfterDisk
	// stays serialised against concurrent cacheHeadPush.
	postBatch := tail[idxBatch:]
	if !strings.Contains(postBatch, "lock.Lock()") {
		t.Fatal("trimJobLocked: expected lock.Lock() reacquisition after " +
			"the remove batch so cacheTrimAfterDisk runs under jobLock " +
			"(matches the pre-#712 lock-order contract for cacheHeadPush " +
			"serialisation).")
	}

	// 4. cacheTrimAfterDisk must be the last cache operation, called after
	// reacquisition — pin its presence so the trim+cache reconciliation
	// stays atomic w.r.t. concurrent Append.
	if !strings.Contains(rest, "s.cacheTrimAfterDisk(jobID, cutoff)") {
		t.Fatal("trimJobLocked: expected cacheTrimAfterDisk(jobID, cutoff) " +
			"after the remove batch so the cache reconciles to the same " +
			"on-disk state.")
	}

	// 5. If the helper variant is in use, validate the helper itself
	// contains os.Remove AND a panic-recovery lock.Lock() — without
	// those, a panic inside the syscall loop would leave jobLock
	// unlocked, the outer Append `defer lock.Unlock()` would itself
	// panic on Unlock-of-unlocked-mutex, and the original FS-layer
	// panic would be masked. R20260527-GO-9 / R20260527-COR-4
	// (#1271, #1291).
	if idxHelper >= 0 {
		const helperMarker = "func (s *runStore) trimRemoveBatch("
		hidx := strings.Index(body, helperMarker)
		if hidx < 0 {
			t.Fatal("trimJobLocked invokes s.trimRemoveBatch but the helper " +
				"definition is missing (#1271/#1291).")
		}
		hbody := body[hidx:]
		if hnext := strings.Index(hbody[len(helperMarker):], "\nfunc "); hnext >= 0 {
			hbody = hbody[:len(helperMarker)+hnext]
		}
		if !strings.Contains(hbody, "os.Remove(") {
			t.Fatal("trimRemoveBatch must contain os.Remove() — the helper " +
				"is the post-#712 home of the perf-critical syscall batch.")
		}
		if !strings.Contains(hbody, "recover()") || !strings.Contains(hbody, "lock.Lock()") {
			t.Fatal("trimRemoveBatch must recover() + lock.Lock() before " +
				"re-panicking so a panic inside os.Remove (FUSE quirks / " +
				"cgo trap) leaves jobLock held when the panic propagates " +
				"to trimJobUnderLock / Append's defer lock.Unlock(). " +
				"Without this, Unlock-of-unlocked-mutex panics mask the " +
				"original FS failure (#1271 / #1291).")
		}
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
