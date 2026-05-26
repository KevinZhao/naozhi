package cron

import (
	"path/filepath"
	"testing"
)

// TestKnownSessionID_LookupAndIsExcluded pins the R242-ARCH-23 (#767)
// contract: LookupKnownSessionID is a behaviour-identical alias of
// IsExcluded. The two MUST agree on every input — including empty
// sessionID, nil receiver, fast-path hits, and slow-path misses — so a
// future caller that picks the LookupKnownSessionID name does not get
// surprised by a divergent implementation. The test runs both methods
// over the same fixture and compares results pointwise.
func TestKnownSessionID_LookupAndIsExcluded(t *testing.T) {
	t.Parallel()

	t.Run("nil_receiver", func(t *testing.T) {
		t.Parallel()
		var s *Scheduler
		if s.IsExcluded("anything") || s.LookupKnownSessionID("anything") {
			t.Fatalf("nil receiver must return false for both methods")
		}
	})

	t.Run("empty_sessionID", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := NewScheduler(SchedulerConfig{
			StorePath: filepath.Join(dir, "cron.json"),
			MaxJobs:   2,
		})
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer s.Stop()
		if s.IsExcluded("") || s.LookupKnownSessionID("") {
			t.Fatalf("empty sessionID must return false for both methods")
		}
	})

	t.Run("fast_path_hit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := NewScheduler(SchedulerConfig{
			StorePath: filepath.Join(dir, "cron.json"),
			MaxJobs:   2,
		})
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer s.Stop()
		j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		const sid = "lookup-aaaa-bbbb-cccc-000000000001"
		s.mu.Lock()
		s.jobs[j.ID].LastSessionID = sid
		s.mu.Unlock()

		if got := s.IsExcluded(sid); !got {
			t.Fatalf("IsExcluded(%q) = false, want true (LastSessionID seed)", sid)
		}
		if got := s.LookupKnownSessionID(sid); !got {
			t.Fatalf("LookupKnownSessionID(%q) = false, want true (must mirror IsExcluded)", sid)
		}
	})

	t.Run("slow_path_miss", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := NewScheduler(SchedulerConfig{
			StorePath: filepath.Join(dir, "cron.json"),
			MaxJobs:   2,
		})
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer s.Stop()
		j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		const sid = "never-seen-aaaa-bbbb-cccc-000000000099"
		isExcl := s.IsExcluded(sid)
		lookup := s.LookupKnownSessionID(sid)
		if isExcl || lookup {
			t.Fatalf("unseen sessionID returned excluded: IsExcluded=%v Lookup=%v", isExcl, lookup)
		}
		if isExcl != lookup {
			t.Fatalf("methods disagree on miss: IsExcluded=%v vs Lookup=%v", isExcl, lookup)
		}
	})
}
