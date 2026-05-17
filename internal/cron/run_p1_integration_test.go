package cron

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestP1_FinishRunPersistsCronRun: a non-skipPersist terminal goes to
// runs/<jobID>/<run_id>.json and is readable via Get / List.
func TestP1_FinishRunPersistsCronRun(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("runStore should be enabled when StorePath is set")
	}

	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	runID := generateRunID()
	startedAt := time.Now().Add(-5 * time.Second)
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: TriggerScheduled,
		state: RunStateSucceeded, sessionID: "sess-AAA", result: "hello",
		prompt: "do thing", workDir: "/tmp/wd", fresh: false,
	})

	got, err := s.GetRun(jobID, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != RunStateSucceeded {
		t.Errorf("state: got %q want %q", got.State, RunStateSucceeded)
	}
	if got.Result != "hello" {
		t.Errorf("result: got %q", got.Result)
	}
	if got.SessionID != "sess-AAA" {
		t.Errorf("session_id: got %q", got.SessionID)
	}
	if got.Prompt != "do thing" {
		t.Errorf("prompt snapshot: got %q", got.Prompt)
	}

	// List path should also surface the run.
	rows := s.ListRuns(jobID, 10, time.Time{})
	if len(rows) != 1 || rows[0].RunID != runID {
		t.Fatalf("ListRuns: got %+v", rows)
	}
}

// TestP1_FinishRunSkipPersistDoesNotWriteHistory: canceled / shutdown
// paths set skipPersist=true → no runs/<jobID>/<run_id>.json written.
func TestP1_FinishRunSkipPersistDoesNotWriteHistory(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	runID := generateRunID()
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: time.Now(), trigger: TriggerScheduled,
		state: RunStateCanceled, errClass: ErrClassCanceled,
		errMsg: "context canceled", skipPersist: true,
	})

	if _, err := s.GetRun(jobID, runID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
	rows := s.ListRuns(jobID, 10, time.Time{})
	if len(rows) != 0 {
		t.Fatalf("expected zero rows for skipPersist run; got %+v", rows)
	}
}

// TestP1_FinishRunSanitisationConsistency: the result/errMsg byte content
// in the persisted CronRun matches what recordResultP0 wrote into
// Job.LastResult / Job.LastError. Otherwise dashboard list view and
// timeline detail would render different bytes for the same run.
func TestP1_FinishRunSanitisationConsistency(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	// Errors with absolute paths trigger redactPathsInCronError.
	rawErr := "session error: open /etc/secret-config: permission denied"
	runID := generateRunID()
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: time.Now(), trigger: TriggerScheduled,
		state: RunStateFailed, errClass: ErrClassSessionError, errMsg: rawErr,
		prompt: "x", workDir: "/wd", fresh: false,
	})

	got, err := s.GetRun(jobID, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	s.mu.RLock()
	jobLastErr := s.jobs[jobID].LastError
	s.mu.RUnlock()

	if got.ErrorMsg != jobLastErr {
		t.Errorf("CronRun.ErrorMsg %q diverges from Job.LastError %q", got.ErrorMsg, jobLastErr)
	}
	// Verify path was redacted (regression guard against double-redact).
	if got.ErrorMsg == rawErr {
		t.Errorf("ErrorMsg not redacted: %q", got.ErrorMsg)
	}
}

// TestP1_DeleteJobByIDRemovesRunsSubtree: deleting a job clears its
// runs/<jobID>/ tree synchronously. ~/.claude/projects/... JSONL is not
// touched (we cannot test that here without a real router; doc-level
// guarantee).
func TestP1_DeleteJobByIDRemovesRunsSubtree(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	// Append one run so the subtree exists.
	s.finishRun(finishArgs{
		job: j, runID: generateRunID(), startedAt: time.Now(),
		trigger: TriggerScheduled, state: RunStateSucceeded, result: "x",
	})
	subtree := filepath.Join(tmp, "runs", jobID)
	if _, err := os.Stat(subtree); err != nil {
		t.Fatalf("subtree should exist after append: %v", err)
	}

	// Delete via the public API.
	if _, err := s.DeleteJobByID(jobID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if _, err := os.Stat(subtree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("subtree should be gone, got err=%v", err)
	}
}

// TestP1_StartTrimAllReclaimsStaleRuns: runs/<jobID>/ entries older than
// keepWindow are removed during Start's cold pass. Mirrors a long
// process-down period.
func TestP1_StartTrimAllReclaimsStaleRuns(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	// Append 3 runs, then push their mtimes to 60 days ago.
	old := time.Now().Add(-60 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		s.finishRun(finishArgs{
			job: j, runID: generateRunID(), startedAt: time.Now(),
			trigger: TriggerScheduled, state: RunStateSucceeded, result: "x",
		})
	}
	subtree := filepath.Join(tmp, "runs", jobID)
	entries, err := os.ReadDir(subtree)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		path := filepath.Join(subtree, e.Name())
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	// trimAll with default 30-day window should clear them all.
	s.runStore.trimAll(time.Now())
	entries2, err := os.ReadDir(subtree)
	if err != nil {
		t.Fatalf("readdir2: %v", err)
	}
	if len(entries2) != 0 {
		t.Errorf("trimAll should reap stale entries; got %d remaining", len(entries2))
	}
}

// TestP1_RecentRunsSurfacesNewestFirst: dashboard list view's recent_runs
// path returns newest entries via Recent helper.
func TestP1_RecentRunsSurfacesNewestFirst(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	// Disable auto-trim so all 5 entries persist regardless of clock skew.
	s.runStore.enableTrimGC = false

	type rec struct {
		runID string
		mtime time.Time
	}
	recs := make([]rec, 0, 5)
	for i := 0; i < 5; i++ {
		runID := generateRunID()
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: time.Now().Add(time.Duration(i) * time.Second),
			trigger: TriggerScheduled, state: RunStateSucceeded, result: "x",
		})
		// Force monotonic mtime for deterministic newest-first ordering on
		// fast filesystems where ctime resolution may collapse adjacent
		// writes.
		mt := time.Now().Add(time.Duration(i) * time.Second)
		path := filepath.Join(tmp, "runs", jobID, runID+".json")
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		recs = append(recs, rec{runID: runID, mtime: mt})
	}

	got := s.RecentRuns(jobID, 3)
	if len(got) != 3 {
		t.Fatalf("recent: want 3, got %d", len(got))
	}
	// Newest-first: recs[4] then recs[3] then recs[2].
	wantOrder := []string{recs[4].runID, recs[3].runID, recs[2].runID}
	for i, want := range wantOrder {
		if got[i].RunID != want {
			t.Errorf("recent[%d]: got %q want %q", i, got[i].RunID, want)
		}
	}
}

// TestP1_DisabledStoreNoOps: NewScheduler with empty StorePath has a
// disabled runStore — Append paths inside finishRun never panic and
// ListRuns / GetRun return empty.
func TestP1_DisabledStoreNoOps(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}})
	if s.runStore == nil || !s.runStore.disabled {
		t.Fatal("runStore should be disabled when StorePath is empty")
	}
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	// Should not panic.
	s.finishRun(finishArgs{
		job: j, runID: generateRunID(), startedAt: time.Now(),
		trigger: TriggerScheduled, state: RunStateSucceeded, result: "x",
	})
	if rows := s.ListRuns(jobID, 10, time.Time{}); len(rows) != 0 {
		t.Errorf("disabled list: want empty, got %+v", rows)
	}
	if _, err := s.GetRun(jobID, "abcdef0123456789"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("disabled get: want ErrNotExist, got %v", err)
	}
}

// TestP1_ConcurrentFinishRunSerialised: multiple goroutines hitting
// finishRun on the same job (CAS-violating overlap is impossible because
// we bypass executeOpt; this only stresses runStore.Append's per-jobID
// mutex). All records persist, none lost. Race-clean under -race.
func TestP1_ConcurrentFinishRunSerialised(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}, StorePath: storePath})
	// Disable trim so we can observe all writes.
	s.runStore.enableTrimGC = false
	jobID := generateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	const N = 30
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			s.finishRun(finishArgs{
				job: j, runID: generateRunID(), startedAt: time.Now(),
				trigger: TriggerScheduled, state: RunStateSucceeded, result: "x",
			})
		}()
	}
	wg.Wait()

	rows := s.ListRuns(jobID, 200, time.Time{})
	if len(rows) != N {
		t.Errorf("concurrent finishRun: want %d records, got %d", N, len(rows))
	}
}
