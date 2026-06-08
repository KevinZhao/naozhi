package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDecodeRunsParallel_PooledSlotNoStaleLeak guards R20260607-PERF-012
// (#1924): the []decodeSlot scratch slice is now recycled via decodeSlotPool
// instead of freshly allocated on every cold-cache warm. The correctness risk
// of pooling is a stale entry leaking from a prior (larger) call into a later
// (smaller) one. We run a large batch first (warms the pool with a big backing
// array carrying ok=true/summary entries), then a smaller batch on a fresh job
// and assert the smaller result holds exactly its own runs in newest-first
// order — no extra or stale rows.
func TestDecodeRunsParallel_PooledSlotNoStaleLeak(t *testing.T) {
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false

	mkJob := func(count int) (string, []string) {
		jobID := mustGenerateID()
		base := time.Now().Add(-time.Hour)
		ids := make([]string, count)
		for i := 0; i < count; i++ {
			run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
			s.Append(run)
			// Stagger mtime so newest-first == highest index; scanSortedRunDir
			// sorts on file mtime, so set it explicitly rather than relying on
			// write-time wall-clock granularity.
			path := filepath.Join(s.root, jobID, run.RunID+".json")
			mt := base.Add(time.Duration(i) * time.Minute)
			if err := os.Chtimes(path, mt, mt); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
			ids[count-1-i] = run.RunID // newest-first order
		}
		return jobID, ids
	}

	// First (large) call warms decodeSlotPool with a big backing array of
	// ok=true slots.
	bigID, bigIDs := mkJob(diskDecodeParallelThreshold + 40)
	bigRows, _, _ := s.diskListNewestFirst(bigID, 200, time.Time{})
	if len(bigRows) != len(bigIDs) {
		t.Fatalf("big rows = %d want %d", len(bigRows), len(bigIDs))
	}

	// Second (smaller) call must reuse the pooled slice but see no stale rows.
	smallID, smallIDs := mkJob(diskDecodeParallelThreshold + 2)
	smallRows, corrupt, unreadable := s.diskListNewestFirst(smallID, 200, time.Time{})
	if corrupt != 0 || unreadable != 0 {
		t.Fatalf("corrupt=%d unreadable=%d want 0/0", corrupt, unreadable)
	}
	if len(smallRows) != len(smallIDs) {
		t.Fatalf("small rows = %d want %d (stale leak from pooled slots?)", len(smallRows), len(smallIDs))
	}
	for i, want := range smallIDs {
		if smallRows[i].RunID != want {
			t.Fatalf("smallRows[%d].RunID = %q want %q", i, smallRows[i].RunID, want)
		}
	}
}

// TestDecodeRunsParallel_PooledSlotReusePreservesCorrectness runs the parallel
// decode path repeatedly on the same job so the pooled []decodeSlot is reused
// many times. Each iteration must return the full set newest-first with no
// drift — proving the per-call clear() resets the recycled backing array.
// R20260607-PERF-012 (#1924).
func TestDecodeRunsParallel_PooledSlotReusePreservesCorrectness(t *testing.T) {
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false

	jobID := mustGenerateID()
	const count = diskDecodeParallelThreshold + 12
	base := time.Now().Add(-time.Hour)
	want := make([]string, count)
	for i := 0; i < count; i++ {
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(run)
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		want[count-1-i] = run.RunID
	}

	for iter := 0; iter < 5; iter++ {
		rows, corrupt, unreadable := s.diskListNewestFirst(jobID, 200, time.Time{})
		if corrupt != 0 || unreadable != 0 {
			t.Fatalf("iter %d: corrupt=%d unreadable=%d want 0/0", iter, corrupt, unreadable)
		}
		if len(rows) != count {
			t.Fatalf("iter %d: rows = %d want %d", iter, len(rows), count)
		}
		for i, w := range want {
			if rows[i].RunID != w {
				t.Fatalf("iter %d: rows[%d].RunID = %q want %q", iter, i, rows[i].RunID, w)
			}
		}
	}
}
