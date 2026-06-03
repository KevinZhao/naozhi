package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_WarmCacheLocked_UnreadableNotCorrupt pins R20260603150052-CR-7
// (#1693): an IO-unreadable file (EACCES simulated via chmod 0000) must be
// counted in unreadableCount, NOT in corruptCount, so warmCache logs the
// correct signal to operators. Before the fix both classes were merged into
// corruptCount, causing "skipped corrupt files" to fire for transient IO
// barriers unrelated to JSON integrity.
//
// The test exercises the decodeRunsParallel code path (> diskDecodeParallelThreshold
// candidates) to ensure the slot.unreadable branch is reached.
func TestRunStore_WarmCacheLocked_UnreadableNotCorrupt(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod 0000 does not deny access")
	}
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// Append enough runs to exceed diskDecodeParallelThreshold so the
	// parallel decode path is exercised.
	const total = diskDecodeParallelThreshold + 4
	now := time.Now()
	runIDs := make([]string, total)
	for i := 0; i < total; i++ {
		r := makeRun(jobID, now.Add(time.Duration(i)*time.Second))
		s.Append(r)
		runIDs[i] = r.RunID
	}

	// Make one run unreadable (EACCES) — IO error, not corrupt JSON.
	unreadablePath := filepath.Join(s.root, jobID, runIDs[0]+".json")
	if err := os.Chmod(unreadablePath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadablePath, 0o600) })

	// Force cold cache so warmCacheLocked scans disk.
	s.cacheInvalidate(jobID)

	corruptCount, unreadableCount := s.warmCacheLocked(jobID)
	if corruptCount != 0 {
		t.Fatalf("corruptCount=%d want 0 — EACCES is not JSON corruption (R20260603150052-CR-7)", corruptCount)
	}
	if unreadableCount != 1 {
		t.Fatalf("unreadableCount=%d want 1 — unreadable file must be counted separately (R20260603150052-CR-7)", unreadableCount)
	}
}
