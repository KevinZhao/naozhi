package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDiskListNewestFirst_SmallLimitOverLargeDir pins R249-PERF-8 (#929):
// a no-cutoff query with a small limit over a directory whose entry count
// exceeds diskDecodeParallelThreshold must (a) return exactly `limit`
// newest-first summaries and (b) stay correct now that the parallel-path
// gate keys on the effective read count min(limit, len) rather than the
// raw directory size. We can't directly observe which path ran, so we
// assert the externally-visible contract: identical newest-first results
// regardless of the internal serial/parallel choice.
func TestDiskListNewestFirst_SmallLimitOverLargeDir(t *testing.T) {
	t.Parallel()
	// keepCount large enough that List won't clamp our small limit.
	s := newTestStore(t, 500, 30*24*time.Hour)
	jobID := mustGenerateID()
	dir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write more than the parallel threshold so the OLD gate (len(items) >
	// threshold) would have taken the parallel path; the new gate keeps a
	// small-limit query serial. Either way the answer must match.
	const total = diskDecodeParallelThreshold + 20
	base := time.Now()
	wantNewest := make([]string, 0, total)
	// index 0 = oldest, total-1 = newest (largest mtime).
	for i := 0; i < total; i++ {
		rid := mustGenerateRunID()
		run := CronRun{JobID: jobID, RunID: rid, StartedAt: base.Add(time.Duration(i) * time.Second)}
		data, err := json.Marshal(&run)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		p := filepath.Join(dir, rid+".json")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		wantNewest = append([]string{rid}, wantNewest...) // prepend → newest-first
	}

	const limit = 5
	got, _ := s.diskListNewestFirst(jobID, limit, time.Time{})
	if len(got) != limit {
		t.Fatalf("got %d summaries, want %d", len(got), limit)
	}
	for i := 0; i < limit; i++ {
		if got[i].RunID != wantNewest[i] {
			t.Fatalf("position %d: got runID %s want %s (newest-first order broken)", i, got[i].RunID, wantNewest[i])
		}
	}
}
