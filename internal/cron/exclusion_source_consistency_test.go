package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// TestExclusionSourceConsistency: KnownSessionIDs, the one cron session
// exclusion entry point, covers every source a cron session ID lives in —
// Job.LastSessionID, the in-flight runs and runStore.Recent — so no cron JSONL
// leaks into the dashboard history panel (#1051).
func TestExclusionSourceConsistency(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	if s.runStore == nil || !s.runStore.layout.Enabled() {
		t.Fatal("test precondition: runStore must be enabled to exercise the slow-path source")
	}

	job := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Source 1 (cheap fast path): Job.LastSessionID.
	const lastSessionID = "src1-last-aaaa-bbbb-cccc-000000000001"
	s.tblForTest().mu.Lock()
	s.tblForTest().jobs[job.ID].LastSessionID = lastSessionID
	s.tblForTest().mu.Unlock()

	// Source 2 (cold-build only): a persisted run's SessionID that lives
	// ONLY in runStore.Recent — not in LastSessionID, not in-flight. This is
	// the case where the single-key probe's cheap sources all miss and it
	// must fall through to the same full build KnownSessionIDs walks.
	const runStoreSessionID = "src2-runstore-dddd-eeee-ffff-000000000002"
	s.runStore.Append(&CronRun{
		JobID:     job.ID,
		RunID:     "abcdef0123456789",
		SessionID: runStoreSessionID,
		StartedAt: time.Unix(2000, 0),
		EndedAt:   time.Unix(2001, 0),
		State:     RunStateSucceeded,
	})
	// Append invalidates the TTL cache; force a fully cold start so each probe
	// below independently re-derives from sources rather than a warm snapshot.
	s.invalidateKnownSessionsCache()

	const neverSeen = "absent-9999-9999-9999-000000000099"

	cases := []struct {
		name      string
		sessionID string
		want      bool
	}{
		{"last_session_id_fast_path", lastSessionID, true},
		{"runstore_only_slow_path", runStoreSessionID, true},
		{"never_seen", neverSeen, false},
		{"empty", "", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Re-cold the cache so each probe exercises the full build.
			s.invalidateKnownSessionsCache()
			if _, got := s.KnownSessionIDs()[tc.sessionID]; got != tc.want {
				t.Fatalf("KnownSessionIDs()[%q] = %v, want %v (#1051)", tc.sessionID, got, tc.want)
			}
		})
	}
}
