package cron

import (
	"sync"
	"testing"
	"time"
)

// TestTrimJobLocked_RecentNotBlockedByTrim is the #502 closing pin: the
// runStore Recent / List path must serve from recentCache without
// acquiring per-job jobLock, so a slow trimJobLocked (200ms+ on FUSE/NFS
// per the issue's symptom report) does NOT block dashboard 1Hz Recent
// reads. The two-phase trim (collect-under-lock, Remove-unlocked) was
// landed under R246-GO-20 / #712; this test pins that Recent observes
// the cache fast-path and never queues behind the os.Remove batch.
//
// R236-GO-08 (#502) — closing pin.
func TestTrimJobLocked_RecentNotBlockedByTrim(t *testing.T) {
	const keepCount = 10
	s := newTestStore(t, keepCount, time.Hour)
	jobID := mustGenerateID()

	// Pre-seed enough runs to make a real trim + warm the cache.
	base := time.Now().Add(-30 * time.Minute)
	for i := 0; i < keepCount*2; i++ {
		s.Append(makeRun(jobID, base.Add(time.Duration(i)*time.Second)))
	}
	// Warm the cache via Recent so subsequent reads serve without IO.
	if rows := s.Recent(jobID, keepCount); len(rows) == 0 {
		t.Fatalf("Recent returned no rows after seed")
	}

	// Hold jobLock externally to simulate a slow trim/Append in flight.
	// If Recent acquired jobLock, it would queue behind us. The cache
	// fast-path means it should return immediately.
	lock := s.jobLock(jobID)
	lock.Lock()

	done := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rows := s.Recent(jobID, keepCount)
		done <- len(rows)
	}()

	// Recent should complete quickly because it serves from recentCache
	// without taking jobLock. Allow generous slack for CI noise; the
	// regression mode (Recent acquires jobLock) wedges indefinitely
	// because we hold the lock.
	select {
	case n := <-done:
		if n == 0 {
			lock.Unlock()
			wg.Wait()
			t.Fatalf("Recent returned 0 rows while cache was warm — broken cache fast-path?")
		}
	case <-time.After(2 * time.Second):
		// Recent is stuck waiting for jobLock — exactly the regression
		// #502 was concerned about. Release the lock so the test exits
		// cleanly before failing.
		lock.Unlock()
		wg.Wait()
		t.Fatal("Recent blocked >2s while jobLock was held externally — " +
			"the cache fast-path no longer covers the warm read. #502 " +
			"closing pin: Recent must serve from recentCache without " +
			"acquiring jobLock, otherwise a slow trimJobLocked (FUSE/NFS) " +
			"queues every dashboard 1Hz poll behind the trim.")
	}
	lock.Unlock()
	wg.Wait()
}
