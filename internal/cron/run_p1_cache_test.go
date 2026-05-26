package cron

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestR220Perf1_RecentCacheHitsAvoidDiskIO: after a single Append warms
// the cache, subsequent Recent calls should not re-stat the runs/<jobID>/
// directory. Verified by deleting the on-disk file post-warm and confirming
// Recent still returns the run (cache served).
func TestR220Perf1_RecentCacheHitsAvoidDiskIO(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 0, 0)

	jobID := generateID()
	runID := generateRunID()
	run := &CronRun{
		RunID: runID, JobID: jobID, State: RunStateSucceeded,
		StartedAt: time.Now(), EndedAt: time.Now(),
	}
	s.Append(run)

	// First Recent — warms cache from disk.
	first := s.Recent(jobID, 10)
	if len(first) != 1 || first[0].RunID != runID {
		t.Fatalf("first Recent: got %+v", first)
	}

	// Now delete the on-disk file. If cache is correctly serving, Recent
	// will still return the same record. If cache miss falls through to
	// disk every call, len would be 0.
	if err := os.Remove(filepath.Join(tmp, "runs", jobID, runID+".json")); err != nil {
		t.Fatalf("remove disk file: %v", err)
	}
	cached := s.Recent(jobID, 10)
	if len(cached) != 1 || cached[0].RunID != runID {
		t.Errorf("cache miss after disk delete; got %+v want 1 entry served from cache", cached)
	}
}

// TestR220Perf1_AppendPushesCacheHeadInOrder: a 2nd Append should appear
// at index 0 of the Recent slice (newest first), not 1.
func TestR220Perf1_AppendPushesCacheHeadInOrder(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 0, 0)
	s.enableTrimGC = false

	jobID := generateID()
	first := generateRunID()
	second := generateRunID()
	s.Append(&CronRun{RunID: first, JobID: jobID, State: RunStateSucceeded, StartedAt: time.Now().Add(-time.Hour)})
	// Warm cache via Recent before second Append.
	_ = s.Recent(jobID, 10)
	s.Append(&CronRun{RunID: second, JobID: jobID, State: RunStateSucceeded, StartedAt: time.Now()})

	got := s.Recent(jobID, 10)
	if len(got) < 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].RunID != second {
		t.Errorf("newest first violated: got[0]=%q want %q", got[0].RunID, second)
	}
	if got[1].RunID != first {
		t.Errorf("got[1]=%q want %q", got[1].RunID, first)
	}
}

// TestR220Perf1_DeleteJobInvalidatesCache: after DeleteJob, Recent
// returns nil even though the entry was previously cached.
func TestR220Perf1_DeleteJobInvalidatesCache(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 0, 0)

	jobID := generateID()
	s.Append(&CronRun{RunID: generateRunID(), JobID: jobID, State: RunStateSucceeded, StartedAt: time.Now()})
	if got := s.Recent(jobID, 10); len(got) != 1 {
		t.Fatalf("warm: got %d entries, want 1", len(got))
	}

	s.DeleteJob(jobID)
	if got := s.Recent(jobID, 10); len(got) != 0 {
		t.Errorf("post-DeleteJob: cache should be empty; got %d entries", len(got))
	}
}

// TestR220Perf1_BeforeCutoffBypassesCache: List with non-zero before falls
// through to disk so paginated queries beyond cache-cap can still scan
// older entries. Verified by populating > keepCount entries (forcing some
// off cache) and querying with before-cutoff.
func TestR220Perf1_BeforeCutoffBypassesCache(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 5, time.Hour) // keepCount=5
	s.enableTrimGC = false                                               // 不让 trim 干扰

	jobID := generateID()
	now := time.Now()
	for i := 0; i < 8; i++ {
		runID := generateRunID()
		startedAt := now.Add(time.Duration(i) * time.Minute)
		s.Append(&CronRun{
			RunID: runID, JobID: jobID, State: RunStateSucceeded,
			StartedAt: startedAt,
		})
		// 强制 mtime 单调递增，保证 List 排序确定。
		path := filepath.Join(tmp, "runs", jobID, runID+".json")
		_ = os.Chtimes(path, startedAt, startedAt)
	}
	// Non-zero before should NOT hit cache — verify by asking for entries
	// older than now+5min; cache only holds 5 newest (= entries 3..7).
	// Note: keepCount=5 means cache capped at 5; entries 0,1,2 only on disk.
	cutoff := now.Add(3*time.Minute + 30*time.Second) // between entry 3 and 4
	rows := s.List(jobID, 10, cutoff)
	// Should see entries 0..3 (StartedAt < cutoff); 4 entries.
	if len(rows) != 4 {
		t.Fatalf("before-cutoff: got %d rows, want 4: %s", len(rows), summariesDesc(rows))
	}
}

// TestR243Perf5_BeforeCutoffCacheTail (#810): when the on-disk count is
// below keepCount the cache is exhaustive, so List(before≠0) can serve
// from cache without ReadDir. Verified by warming cache, deleting all
// disk files, then querying with a before-cutoff: cache should still
// answer because count<cap.
func TestR243Perf5_BeforeCutoffCacheTail(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 10, time.Hour) // keepCount=10
	s.enableTrimGC = false

	jobID := generateID()
	now := time.Now()
	for i := 0; i < 5; i++ { // 5 < keepCount=10 → cache is exhaustive
		runID := generateRunID()
		startedAt := now.Add(time.Duration(i) * time.Minute)
		s.Append(&CronRun{
			RunID: runID, JobID: jobID, State: RunStateSucceeded,
			StartedAt: startedAt,
		})
		path := filepath.Join(tmp, "runs", jobID, runID+".json")
		_ = os.Chtimes(path, startedAt, startedAt)
	}
	// Warm cache.
	if got := s.Recent(jobID, 10); len(got) != 5 {
		t.Fatalf("warm: got %d entries, want 5", len(got))
	}
	// Delete disk files. If List served from disk it would return 0.
	dir := filepath.Join(tmp, "runs", jobID)
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		_ = os.Remove(filepath.Join(dir, f.Name()))
	}
	// Query before-cutoff between entry 1 and 2; should see entries 0, 1.
	cutoff := now.Add(time.Minute + 30*time.Second)
	rows := s.List(jobID, 10, cutoff)
	if len(rows) != 2 {
		t.Fatalf("cache tail before-cutoff: got %d rows, want 2: %s", len(rows), summariesDesc(rows))
	}
}

func summariesDesc(rs []CronRunSummary) string {
	out := ""
	for i, r := range rs {
		if i > 0 {
			out += " | "
		}
		out += r.RunID + "@" + strconv.FormatInt(r.StartedAt.UnixMilli(), 10)
	}
	return out
}
