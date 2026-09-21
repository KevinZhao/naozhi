package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_Append_HappyPathAfterGuard asserts the guard does NOT block
// a legitimate hex JobID — Append still writes the run record under the
// per-job subdir.
func TestRunStore_Append_HappyPathAfterGuard(t *testing.T) {
	s := newTestStore(t, 5, time.Hour)
	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	s.Append(run)
	wantPath := filepath.Join(s.rootDir(), jobID, run.RunID+".json")
	if _, err := os.Lstat(wantPath); err != nil {
		t.Fatalf("expected Append to write %q: %v", wantPath, err)
	}
}
