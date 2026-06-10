package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// TestScheduler_Start_PrewarmsRecentCacheOnRestart pins R250-PERF-9 (#1112)
// at the Scheduler.Start integration level (the unit test
// TestRunStore_TrimAll_PrewarmsRecentCache exercises trimAllCtx in
// isolation; this one proves the cold-start GC goroutine Start() spawns
// actually drives the pre-warm on a process restart).
//
// Scenario: a first scheduler persists a job + one run record to a shared
// StorePath, then a SECOND scheduler boots against the same StorePath
// (simulating a process restart). After Start()'s cold-start GC goroutine
// finishes (gcWG.Wait), the restarted scheduler's recentCache for the
// reloaded job must already be warm — so the first dashboard RecentRuns
// poll hits the cache instead of cold-warming on the request path.
func TestScheduler_Start_PrewarmsRecentCacheOnRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")

	// --- First boot: create a job + persist a run record to disk. ---
	s1 := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	if err := s1.Start(); err != nil {
		t.Fatalf("s1 Start: %v", err)
	}
	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "ping",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true, // no live cron entry needed
	}
	if err := s1.AddJob(j); err != nil {
		t.Fatalf("s1 AddJob: %v", err)
	}
	jobID := j.ID
	s1.runStore.Append(makeRun(jobID, time.Now().Add(-30*time.Minute)))
	s1.gcWG.Wait() // let s1's own cold-start GC settle before Stop
	s1.Stop()

	// --- Restart: second scheduler boots against the same StorePath. ---
	s2 := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	if err := s2.Start(); err != nil {
		t.Fatalf("s2 Start: %v", err)
	}
	t.Cleanup(s2.Stop)

	// The job must have been reloaded from cron_jobs.json.
	s2.mu.RLock()
	_, loaded := s2.jobs[jobID]
	s2.mu.RUnlock()
	if !loaded {
		t.Fatalf("restart did not reload job %s from %s", jobID, storePath)
	}

	// Wait for the cold-start GC goroutine (which now also pre-warms) to
	// finish, then assert the cache entry is warm WITHOUT calling cacheGet
	// (which would itself lazily warm and mask a regression).
	s2.gcWG.Wait()

	v, ok := s2.runStore.recentCache.Load(jobID)
	if !ok {
		t.Fatal("restart cold-start GC must have pre-warmed (created) the cache entry (#1112)")
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	warm := entry.warm
	count := entry.count
	entry.mu.Unlock()
	if !warm {
		t.Fatal("restart cold-start GC must leave the recentCache entry warm (#1112)")
	}
	if count == 0 {
		t.Fatal("pre-warmed cache should hold the persisted run row from before the restart")
	}
}
