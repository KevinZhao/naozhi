package cron

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDeleteJobByID_PersistFailureCleansRunsDir is a regression test for
// R236-GO-04 (#495): when persistJobsLocked fails inside DeleteJobByID,
// the in-memory delete already happened — if runStore.DeleteJob does NOT
// also fire on the persist-failure path, the runs/<jobID>/ subtree
// remains on disk. A subsequent AddJob that reuses the same ID (16-hex
// generator + tiny but non-zero collision probability) inherits the old
// job's run history.
//
// The fix lives in scheduler_jobs.go DeleteJobByID's postCleanup, which
// runs lock-free AFTER op + persist but BEFORE the persist error is
// returned to the caller. withJobByIDOpt only skips postCleanup when
// rolledBack == true, and DeleteJobByID provides no rollbackOnPersistErr,
// so postCleanup fires unconditionally on the persist-failure path.
//
// Test shape:
//  1. Build a Scheduler + seed one job
//  2. Append a fake CronRun directly via runStore so runs/<jobID>/<runID>.json
//     exists on disk
//  3. Install the failing marshaler (so persistJobsLocked errors)
//  4. Call DeleteJobByID — expect ErrPersistFailed
//  5. Assert runs/<jobID>/ no longer exists (postCleanup ran despite perr)
func TestDeleteJobByID_PersistFailureCleansRunsDir(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("test setup precondition: runStore must be enabled")
	}

	// Append a fake run so the runs/<jobID>/ directory + a child file
	// actually exist on disk. Without this the os.Stat below would pass
	// trivially (dir is created lazily on first Append).
	s.runStore.Append(&CronRun{
		JobID:     id,
		RunID:     "1234567890abcdef",
		StartedAt: time.Unix(1000, 0),
		EndedAt:   time.Unix(1001, 0),
		State:     RunStateSucceeded,
	})

	jobDir := filepath.Join(s.runStore.root, id)
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("setup: runs/<jobID>/ should exist after Append: %v", err)
	}

	withFailingMarshal(t, s)

	_, err := s.DeleteJobByID(id)
	if !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("DeleteJobByID err = %v, want ErrPersistFailed", err)
	}

	// The whole point of #495: even though persist failed, postCleanup ran
	// and runs/<jobID>/ is gone — so a future AddJob with the same ID will
	// not inherit stale run history.
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("runs/<jobID>/ still exists after DeleteJobByID with persist failure; "+
			"got Stat err=%v want IsNotExist (#495 regression)", err)
	}
}

// TestDeleteJobByPrefix_PersistFailureCleansRunsDir mirrors the byID
// counterpart for the IM-prefix DeleteJob path. Both paths flow through
// postCleanup → runStore.DeleteJob; the prefix path has its own
// withJobByPrefix helper so a divergence between the two helpers would
// silently leak runs/ on the prefix path while the byID path stays clean.
func TestDeleteJobByPrefix_PersistFailureCleansRunsDir(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)

	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("test setup precondition: runStore must be enabled")
	}

	s.runStore.Append(&CronRun{
		JobID:     id,
		RunID:     "1234567890abcdef",
		StartedAt: time.Unix(1000, 0),
		EndedAt:   time.Unix(1001, 0),
		State:     RunStateSucceeded,
	})

	jobDir := filepath.Join(s.runStore.root, id)
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("setup: runs/<jobID>/ should exist after Append: %v", err)
	}

	withFailingMarshal(t, s)

	// Seeded job is on (feishu, chat1); use a 4-char prefix.
	if _, err := s.DeleteJob(id[:4], "feishu", "chat1"); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("DeleteJob err = %v, want ErrPersistFailed", err)
	}

	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("runs/<jobID>/ still exists after DeleteJob (prefix) with persist failure; "+
			"got Stat err=%v want IsNotExist (#495 regression)", err)
	}
}
