package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_WriteFailedTotalsBumpsOnPermDenied pins R20260527122801-CR-18
// (#1338): when WriteFileAtomic fails (here: parent dir made read-only),
// runStore.writeFailedOtherTotal must increment so /health endpoints and
// tests see an actionable signal that history records were dropped.
//
// Skip-on-root: a root uid bypasses the 0o500 perm gate and the write
// would succeed, defeating the test premise. CI runs as non-root so the
// gate fires; if a developer runs as root locally the skip prevents a
// confusing pass.
func TestRunStore_WriteFailedTotalsBumpsOnPermDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires non-root euid; perm gate is bypassed otherwise")
	}
	t.Parallel()

	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	store := newRunStore(storePath, 10, 24*time.Hour)
	if store.disabled {
		t.Fatal("expected enabled store")
	}

	jobID := "abcdef0123456789" // 16-hex
	runID := "0123456789abcdef"

	// Pre-create the per-job dir and chmod 0o500 so WriteFileAtomic's
	// rename inside it fails (POSIX: rename needs write+execute on the
	// containing dir).
	jobDir := filepath.Join(store.root, jobID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Mark the dir as already-ensured so Append's ensureJobDir hot-path
	// doesn't try to MkdirAll on the read-only dir (which would be the
	// path that errors first; we want the WriteFileAtomic path).
	store.jobDirEnsured.Store(jobID, struct{}{})
	if err := os.Chmod(jobDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(jobDir, 0o700) //nolint:errcheck // best-effort cleanup so t.TempDir RemoveAll works.

	df0, ot0 := store.WriteFailedTotals()
	if df0 != 0 || ot0 != 0 {
		t.Fatalf("baseline counters non-zero: diskFull=%d other=%d", df0, ot0)
	}

	store.Append(&CronRun{
		JobID:     jobID,
		RunID:     runID,
		State:     RunStateSucceeded,
		StartedAt: time.Now(),
		EndedAt:   time.Now(),
	})

	df1, ot1 := store.WriteFailedTotals()
	if ot1 != ot0+1 {
		t.Errorf("writeFailedOtherTotal: want +1, got delta %d (diskFull=%d)", ot1-ot0, df1)
	}
	if df1 != df0 {
		t.Errorf("writeFailedDiskFullTotal must not bump on EACCES: got delta %d", df1-df0)
	}
}

// TestRunStore_WriteFailedTotalsNilSafe pins that the exported accessor
// stays safe to call on a nil receiver — server /health may construct
// the response before the store is initialised.
func TestRunStore_WriteFailedTotalsNilSafe(t *testing.T) {
	t.Parallel()
	var s *runStore
	df, ot := s.WriteFailedTotals()
	if df != 0 || ot != 0 {
		t.Errorf("nil-receiver totals: want (0,0), got (%d,%d)", df, ot)
	}
}
