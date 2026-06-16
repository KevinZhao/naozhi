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

// TestDiskListNewestFirst_ParallelBackfillsOverCorruptInOverCapWindow pins the
// #2150 fix: in the over-cap window (len(items) > limit, e.g. a concurrent warm
// observing count > keepCount before trim) a corrupt/unreadable file among the
// newest `limit` candidates must NOT shrink the result — decodeRunsParallel
// must backfill from older valid candidates until `limit` valid rows are
// gathered, mirroring the serial path's accumulate-until-`limit` walk. Before
// the fix the parallel branch hard-capped at n=min(len,limit) and dropped the
// corrupt slot with no backfill, returning fewer than `limit` rows.
func TestDiskListNewestFirst_ParallelBackfillsOverCorruptInOverCapWindow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// Build a candidate window larger than the requested limit so backfill has
	// older valid candidates to pull from (the over-cap window).
	const limit = diskDecodeParallelThreshold + 2 // > threshold → parallel branch
	const extra = 5                               // older valid candidates to backfill from
	const corruptInWindow = 3                     // corrupt files inside the newest `limit`

	base := time.Now().Add(-time.Hour)
	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	total := limit + extra
	items := make([]runDirItem, total)
	wantValidIDs := make([]string, 0, limit)
	for i := 0; i < total; i++ {
		// newest-first index i: highest mtime first.
		mt := base.Add(time.Duration(total-i) * time.Minute)
		// Make the first `corruptInWindow` (newest) entries corrupt; the rest valid.
		if i < corruptInWindow {
			cid := mustGenerateRunID()
			p := filepath.Join(dir, cid+".json")
			if err := os.WriteFile(p, []byte("{not valid json"), 0o600); err != nil {
				t.Fatalf("WriteFile corrupt: %v", err)
			}
			_ = os.Chtimes(p, mt, mt)
			items[i] = runDirItem{path: p, runID: cid, mtime: mt}
			continue
		}
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		s.Append(run)
		p := filepath.Join(dir, run.RunID+".json")
		_ = os.Chtimes(p, mt, mt)
		items[i] = runDirItem{path: p, runID: run.RunID, mtime: mt}
		if len(wantValidIDs) < limit {
			wantValidIDs = append(wantValidIDs, run.RunID)
		}
	}

	rows, corrupt, unreadable := s.decodeRunsParallel(items, limit)
	if unreadable != 0 {
		t.Fatalf("unreadableCount = %d want 0", unreadable)
	}
	// The 3 corrupt files sit in the scanned prefix needed to gather `limit`
	// valid rows, so they must all be counted.
	if corrupt != corruptInWindow {
		t.Fatalf("corruptCount = %d want %d", corrupt, corruptInWindow)
	}
	// The fix: backfill yields a FULL `limit` rows despite the corrupt files in
	// the newest window (pre-fix this returned limit-corruptInWindow rows).
	if len(rows) != limit {
		t.Fatalf("rows = %d want %d (backfill over corrupt failed)", len(rows), limit)
	}
	for i := range wantValidIDs {
		if rows[i].RunID != wantValidIDs[i] {
			t.Fatalf("rows[%d].RunID = %q want %q (newest-first order/backfill wrong)", i, rows[i].RunID, wantValidIDs[i])
		}
	}
}
