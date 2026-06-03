package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_WarmCacheLocked_ReturnsCorruptCount pins R236-PERF-09
// (#527, partial): the slog.Warn for skipped corrupt files moved out
// of the jobLock + entry.mu critical section. warmCacheLocked is the
// inner critical-section helper that returns the corruptCount (and
// unreadableCount) so the public warmCache wrapper can emit the
// operator-facing slog AFTER the locks drop. Without this test the
// next refactor that drops the return value would silently regress the
// lock-window cleanup.
func TestRunStore_WarmCacheLocked_ReturnsCorruptCount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// Seed one good run so the dir + at least one row exist.
	good := makeRun(jobID, time.Now().Add(-2*time.Hour))
	s.Append(good)

	// Drop a corrupt JSON file alongside it so diskListNewestFirst's
	// scan picks it up and parseRunBytes rejects it as ErrCorruptRun.
	corruptID := mustGenerateID()
	corruptPath := filepath.Join(s.root, jobID, corruptID+".json")
	if err := os.WriteFile(corruptPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt run: %v", err)
	}

	// Force a cold cache so warmCacheLocked actually runs the disk scan.
	s.cacheInvalidate(jobID)

	gotCorrupt, gotUnreadable := s.warmCacheLocked(jobID)
	if gotCorrupt != 1 {
		t.Fatalf("warmCacheLocked corruptCount = %d, want 1", gotCorrupt)
	}
	if gotUnreadable != 0 {
		t.Fatalf("warmCacheLocked unreadableCount = %d, want 0", gotUnreadable)
	}

	// Second call must return 0,0 because the entry is now warm — the
	// "another goroutine warmed it" early-return path. The slog.Warn
	// in the public wrapper would otherwise fire spuriously.
	if c, u := s.warmCacheLocked(jobID); c != 0 || u != 0 {
		t.Fatalf("warmCacheLocked second call corrupt=%d unreadable=%d, want 0,0 (warm fast path)", c, u)
	}
}
