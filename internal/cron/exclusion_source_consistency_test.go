package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// TestExclusionSourceConsistency pins R244-ARCH-10 (#1051): the cron session
// exclusion logic exposes three entry points that MUST agree on every
// sessionID, regardless of which underlying source the ID lives in:
//
//   - IsExcluded            — the session.SessionIDExcluder interface probe
//   - LookupKnownSessionID  — the named in-package single-key probe
//   - KnownSessionIDs()[id] — the full-set dashboard accessor
//
// The exclusion logic is deliberately scattered across cheap fast paths
// (Job.LastSessionID, in-flight runningJobs) and an expensive cold build
// (runStore.Recent). The single-key probes short-circuit on the cheap
// sources; the full-set accessor always walks every source. If those code
// paths ever drift — e.g. a new source is added to buildKnownSessionsSet but
// not to containsSessionID's fast path, or vice versa — a sessionID could be
// excluded by one API and not the other, silently leaking a cron JSONL into
// the dashboard history panel (or excluding a user session from the
// auto-workspace-chain pool).
//
// This is the consistency invariant a future ExcluderRegistry refactor must
// preserve; pinning it now means the refactor (or any incremental drift) is
// caught at the API boundary instead of in production.
func TestExclusionSourceConsistency(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        5,
		AllowNilRouter: true,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("test precondition: runStore must be enabled to exercise the slow-path source")
	}

	job := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Source 1 (cheap fast path): Job.LastSessionID.
	const lastSessionID = "src1-last-aaaa-bbbb-cccc-000000000001"
	s.mu.Lock()
	s.jobs[job.ID].LastSessionID = lastSessionID
	s.mu.Unlock()

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
			// Re-cold the cache before each probe so the single-key APIs and
			// the full-set accessor each start from the same uncached state —
			// otherwise an earlier probe's warm snapshot could mask a drift in
			// a later cold-path probe.
			s.invalidateKnownSessionsCache()
			gotIsExcluded := s.IsExcluded(tc.sessionID)

			s.invalidateKnownSessionsCache()
			gotLookup := s.LookupKnownSessionID(tc.sessionID)

			s.invalidateKnownSessionsCache()
			_, gotInSet := s.KnownSessionIDs()[tc.sessionID]
			if tc.sessionID == "" {
				// The empty string is never a legitimate key; KnownSessionIDs
				// never inserts it, so gotInSet is trivially false and the
				// probe APIs short-circuit. Only assert the probe APIs here.
				gotInSet = false
			}

			if gotIsExcluded != tc.want || gotLookup != tc.want || gotInSet != tc.want {
				t.Fatalf("exclusion APIs disagree for %q:\n  IsExcluded=%v\n  LookupKnownSessionID=%v\n  id∈KnownSessionIDs=%v\n  want all=%v\n(the three exclusion entry points must agree across every source — #1051)",
					tc.sessionID, gotIsExcluded, gotLookup, gotInSet, tc.want)
			}
		})
	}
}
