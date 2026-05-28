package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCleanupRunningJobIfIdle_KnownWindowDoc pins the godoc paragraph that
// documents the remaining narrow split-CAS window after R20260527-GO-2's
// CompareAndDelete tightening. PR #1416 review (R040034-CHANGES on
// internal/cron/scheduler_run.go:932) flagged that the comment before this
// fix claimed CompareAndDelete-on-pointer closed the gap entirely, but it
// only closes Load+CompareAndDelete TOCTOU — the adjacent window where
// executeOpt has already done `inflight := s.jobInflight(j.ID)` and is
// about to CompareAndSwap on the now-stale pointer remains open under
// (DeleteJob racing TriggerNow on same ID) + (16-hex-char ID reuse).
//
// We bake the acknowledgement into the godoc rather than chase the window
// with a per-jobID lock because the residual probability is ~2^-32 over a
// process lifetime at maxJobsHardCap=500. The pin guards against a future
// edit silently dropping the acknowledgement and re-asserting that
// CompareAndDelete is sufficient — a claim that wouldn't survive a
// reviewer who actually traces the executeOpt entry.
func TestCleanupRunningJobIfIdle_KnownWindowDoc(t *testing.T) {
	t.Parallel()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	src, err := os.ReadFile(filepath.Join(dir, "scheduler_run.go"))
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)

	// The R040034-CHANGES anchor pins the issue identifier so future grep
	// for the deferred follow-up lands here. Required co-anchors document
	// the trigger conditions a reader has to know to evaluate severity.
	required := []string{
		"R040034-CHANGES",
		"jobInflight(j.ID)",
		"orphaned old gate",
		"per-jobID lock",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("scheduler_run.go is missing the known-window godoc anchor %q — "+
				"the residual split-CAS window must remain documented at the "+
				"cleanupRunningJobIfIdle call site so future readers don't claim "+
				"CompareAndDelete-on-pointer is fully sufficient.",
				want)
		}
	}
}
