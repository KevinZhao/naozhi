package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWarmCache_UnreadableCountDistinctFromCorrupt pins R20260603-CR-1
// (#1693): warmCacheLocked must return (corruptCount, unreadableCount) as
// separate values so warmCache can emit distinct log messages for data
// corruption vs I/O errors (EACCES/EIO/ESTALE). Before the fix, both were
// lumped into corruptCount, making the "skipped corrupt files" slog message
// misleading for transient filesystem errors.
func TestWarmCache_UnreadableCountDistinctFromCorrupt(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod 0000 does not deny access")
	}
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// Seed two good runs.
	r1 := makeRun(jobID, time.Now().Add(-3*time.Hour))
	r2 := makeRun(jobID, time.Now().Add(-2*time.Hour))
	s.Append(r1)
	s.Append(r2)

	// Add one corrupt file (bad JSON).
	corruptID := mustGenerateRunID()
	corruptPath := filepath.Join(s.root, jobID, corruptID+".json")
	if err := os.WriteFile(corruptPath, []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	// Add one unreadable file (EACCES via chmod 0000).
	unreadableID := mustGenerateRunID()
	unreadablePath := filepath.Join(s.root, jobID, unreadableID+".json")
	if err := os.WriteFile(unreadablePath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write unreadable: %v", err)
	}
	if err := os.Chmod(unreadablePath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadablePath, 0o600) })

	// Force cold cache.
	s.cacheInvalidate(jobID)

	corruptCount, unreadableCount := s.warmCacheLocked(jobID)
	if corruptCount != 1 {
		t.Errorf("corruptCount = %d, want 1 (only ErrCorruptRun files)", corruptCount)
	}
	if unreadableCount != 1 {
		t.Errorf("unreadableCount = %d, want 1 (EACCES file must be counted separately)", unreadableCount)
	}
}
