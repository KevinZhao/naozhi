package cron

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFinishRun_DeleteRaceNoOrphanRunsDir pins #2058: finishRun's terminal
// write is two-step and non-atomic. recordTerminalResult confirms the job
// exists, bumps RunCounters, persists cron_jobs.json, then RELEASES s.mu and
// returns jobPersistOK=true. appendRun then writes the physically-separate
// runs/<jobID>/ store under only its own per-job jobLock (never s.mu), across
// a marshal+fsync window.
//
// A concurrent DeleteJobByID in that window drops the job from s.jobs AND runs
// runStore.DeleteJob → RemoveAll(runs/<jobID>). Without the s.jobs re-check
// added before appendRun, the stale snapshot's appendRun → ensureJobDir would
// MkdirAll the directory back, resurrecting an orphaned runs/<jobID>/ subtree
// for a job that no longer exists in cron_jobs.json (a bounded disk leak that
// retention trimming never reclaims).
//
// The invariant under test: after both finishRun and DeleteJobByID complete,
// if the job is gone from cron_jobs.json then runs/<jobID>/ must NOT contain a
// resurrected run record. Run under `go test -race` and many iterations to
// exercise the interleaving window.
func TestFinishRun_DeleteRaceNoOrphanRunsDir(t *testing.T) {
	const iters = 200

	for i := 0; i < iters; i++ {
		dir := t.TempDir()
		storePath := filepath.Join(dir, "cron.json")
		s := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{})
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}

		j := &Job{
			Schedule: "@every 1h",
			Prompt:   "ping",
			Platform: "feishu",
			ChatID:   "chat1",
			ChatType: "direct",
			Paused:   true, // no live cron entry
		}
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		jobID := j.ID
		runsRoot := filepath.Join(dir, "runs")

		inflight := s.jobInflight(jobID)
		if !inflight.running.CompareAndSwap(false, true) {
			t.Fatal("initial CAS must succeed")
		}
		finalizer := &runFinalizer{inflight: inflight}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.finishRun(finishArgs{
				job:       j,
				runID:     "0123456789abcdef",
				startedAt: time.Now(),
				trigger:   TriggerScheduled,
				state:     RunStateSucceeded,
				sessionID: "sess-1",
				result:    "ok",
				finalizer: finalizer,
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = s.DeleteJobByID(jobID)
		}()
		wg.Wait()

		// If the delete won (job gone from the live map AND on disk), the runs
		// subtree must not have been resurrected behind it.
		if !s.jobStillExists(jobID) {
			jobRunDir := filepath.Join(runsRoot, jobID)
			if entries, err := os.ReadDir(jobRunDir); err == nil {
				for _, e := range entries {
					if filepath.Ext(e.Name()) == ".json" {
						s.Stop()
						t.Fatalf("iter %d: orphaned runs subtree resurrected after job delete: "+
							"found %q in %s (#2058)", i, e.Name(), jobRunDir)
					}
				}
			}
		}
		s.Stop()
	}
}
