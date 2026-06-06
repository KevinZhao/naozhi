package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDiskListNewestFirst_ParallelDecodePreservesOrder appends more than
// diskDecodeParallelThreshold runs so the no-cutoff path fans the decode out
// across the worker pool, then asserts the summaries come back strictly
// newest-first regardless of worker completion order (R247-PERF-9 / #540,
// R249-PERF-7 / #928). scanSortedRunDir sorts by mtime DESC, so we stagger
// each run's file mtime to give a deterministic expected ordering.
func TestDiskListNewestFirst_ParallelDecodePreservesOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	const count = diskDecodeParallelThreshold + 8 // force the parallel branch
	base := time.Now().Add(-time.Hour)
	runIDsByMtime := make([]string, count)
	for i := 0; i < count; i++ {
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(run)
		// Stagger mtime so newest-first == highest index. scanSortedRunDir
		// sorts on file mtime, so set it explicitly rather than relying on
		// write-time wall-clock granularity.
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		runIDsByMtime[count-1-i] = run.RunID // newest-first order
	}

	rows, corrupt, unreadable := s.diskListNewestFirst(jobID, 200, time.Time{})
	if corrupt != 0 {
		t.Fatalf("corruptCount = %d want 0", corrupt)
	}
	if unreadable != 0 {
		t.Fatalf("unreadableCount = %d want 0", unreadable)
	}
	if len(rows) != count {
		t.Fatalf("rows = %d want %d", len(rows), count)
	}
	for i, want := range runIDsByMtime {
		if rows[i].RunID != want {
			t.Fatalf("rows[%d].RunID = %q want %q (order not preserved)", i, rows[i].RunID, want)
		}
	}
}

// TestDiskListNewestFirst_ParallelDecodeSkipsCorrupt ensures the parallel
// branch counts corrupt files exactly like the serial branch and still
// returns the readable rows in newest-first order.
func TestDiskListNewestFirst_ParallelDecodeSkipsCorrupt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	const good = diskDecodeParallelThreshold + 4
	base := time.Now().Add(-time.Hour)
	for i := 0; i < good; i++ {
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(run)
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		mt := base.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(path, mt, mt)
	}

	// Drop two well-formed-name but corrupt-content files into the dir so the
	// candidate count stays above the parallel threshold and the corrupt
	// accounting is exercised on the pooled path.
	for i := 0; i < 2; i++ {
		corruptID := mustGenerateRunID()
		path := filepath.Join(s.root, jobID, corruptID+".json")
		if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
			t.Fatalf("WriteFile corrupt: %v", err)
		}
	}

	rows, corrupt, unreadable := s.diskListNewestFirst(jobID, 200, time.Time{})
	if corrupt != 2 {
		t.Fatalf("corruptCount = %d want 2", corrupt)
	}
	if unreadable != 0 {
		t.Fatalf("unreadableCount = %d want 0", unreadable)
	}
	if len(rows) != good {
		t.Fatalf("rows = %d want %d", len(rows), good)
	}
}

// TestDiskListNewestFirst_ParallelLimitTrimsToNewest verifies that when the
// no-cutoff parallel path is asked for fewer rows than exist it decodes only
// the newest `limit` candidates (not the whole directory) and returns them
// newest-first.
func TestDiskListNewestFirst_ParallelLimitTrimsToNewest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	const count = diskDecodeParallelThreshold + 20
	base := time.Now().Add(-time.Hour)
	runIDsByMtime := make([]string, count)
	for i := 0; i < count; i++ {
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(run)
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		mt := base.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(path, mt, mt)
		runIDsByMtime[count-1-i] = run.RunID
	}

	const limit = diskDecodeParallelThreshold + 1
	rows, _, _ := s.diskListNewestFirst(jobID, limit, time.Time{})
	if len(rows) != limit {
		t.Fatalf("rows = %d want %d", len(rows), limit)
	}
	for i := 0; i < limit; i++ {
		if rows[i].RunID != runIDsByMtime[i] {
			t.Fatalf("rows[%d].RunID = %q want %q", i, rows[i].RunID, runIDsByMtime[i])
		}
	}
}
