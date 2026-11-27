package cron

import (
	"path/filepath"
	"testing"
)

// TestRunningJobsCleanup_DeleteClearsIdle pins R242-ARCH-15 (#758): both
// delete entry points (DeleteJobByID and the IM prefix DeleteJob) reclaim an
// idle job's runningJobs entry. The prior policy left every entry pinned for
// the process lifetime, leaking one *runInflight per deleted jobID. The
// refusal to reclaim a held gate is TestRunGate_AcquireReleaseReclaim's.
func TestRunningJobsCleanup_DeleteClearsIdle(t *testing.T) {
	t.Parallel()
	for name, del := range map[string]func(s *Scheduler, j *Job) error{
		"by_id": func(s *Scheduler, j *Job) error { _, err := s.DeleteJobByID(j.ID); return err },
		"by_prefix": func(s *Scheduler, j *Job) error {
			_, err := s.DeleteJob(j.ID, j.Platform, j.ChatID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 10}, SchedulerDeps{})
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Stop()

			j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "X"}
			if err := s.AddJob(j); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			_ = s.gateForTest().jobInflight(j.ID)
			if _, present := s.gateForTest().runningJobs.Load(j.ID); !present {
				t.Fatalf("precondition: runningJobs entry should exist after jobInflight touch")
			}
			if err := del(s, j); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, present := s.gateForTest().runningJobs.Load(j.ID); present {
				t.Errorf("runningJobs entry should be cleaned up after delete on an idle job")
			}
		})
	}
}
