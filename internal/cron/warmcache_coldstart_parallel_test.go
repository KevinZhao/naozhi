package cron

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWarmCache_ColdStartParallelDecode covers R249-PERF-7 (#928): the FIRST
// dashboard hit on a cold cache warms the ring by reading every on-disk run
// through diskListNewestFirst's no-cutoff path. With more than
// diskDecodeParallelThreshold records that decode now fans out across a
// worker pool, so this test pins the end-to-end contract: a freshly
// constructed store (cold cache, no prior warm) returns the runs newest-first
// with intact payload after the parallel warm.
func TestWarmCache_ColdStartParallelDecode(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	const count = diskDecodeParallelThreshold + 12 // force the parallel branch
	base := time.Now().Add(-2 * time.Hour)
	wantRunIDsNewestFirst := make([]string, count)
	wantSessionByRunID := make(map[string]string, count)
	for i := 0; i < count; i++ {
		run := makeRun(jobID, base.Add(time.Duration(i)*time.Second))
		run.SessionID = fmt.Sprintf("%016x", i) // hex so it survives any validation
		s.Append(run)
		// Stagger mtime so newest-first ordering is deterministic regardless
		// of write wall-clock granularity (scanSortedRunDir sorts on mtime).
		path := filepath.Join(s.root, jobID, run.RunID+".json")
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		wantRunIDsNewestFirst[count-1-i] = run.RunID
		wantSessionByRunID[run.RunID] = run.SessionID
	}

	// Force a genuinely cold cache: drop any entry seeded by Append's
	// cacheHeadPush so Recent must warm from disk via the parallel decode.
	s.cacheInvalidate(jobID)

	got := s.Recent(jobID, count)
	if len(got) != count {
		t.Fatalf("Recent len = %d want %d", len(got), count)
	}
	for i, wantID := range wantRunIDsNewestFirst {
		if got[i].RunID != wantID {
			t.Fatalf("got[%d].RunID = %q want %q (cold-start order wrong)", i, got[i].RunID, wantID)
		}
		if got[i].SessionID != wantSessionByRunID[wantID] {
			t.Fatalf("got[%d].SessionID = %q want %q (payload lost on parallel decode)",
				i, got[i].SessionID, wantSessionByRunID[wantID])
		}
	}

	// Second call must hit the now-warm ring and stay identical — proves the
	// parallel warm seeded the cache, not just produced a one-off slice.
	again := s.Recent(jobID, count)
	if len(again) != count {
		t.Fatalf("warm Recent len = %d want %d", len(again), count)
	}
	for i := range again {
		if again[i].RunID != got[i].RunID {
			t.Fatalf("warm re-read diverged at %d: %q vs %q", i, again[i].RunID, got[i].RunID)
		}
	}
}
