package cron

import "testing"

// TestNewScheduler_AllowedRootNULBytePruned (#1297): a NUL byte is never valid
// in a path, so NewScheduler must strip a NUL-bearing cfg.AllowedRoot at
// construction instead of carrying it into workDir resolution.
func TestNewScheduler_AllowedRootNULBytePruned(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{
		AllowedRoot:    "/tmp/safe\x00/etc",
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if s.allowedRoot != "" {
		t.Fatalf("allowedRoot = %q, want empty (NUL byte should be rejected)", s.allowedRoot)
	}
}
