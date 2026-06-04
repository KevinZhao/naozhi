package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCleanupRunningJobIfIdle_KnownWindowDoc pins the godoc paragraph at the
// cleanupRunningJobIfIdle CompareAndDelete site that documents the
// double-execution window's lifecycle.
//
// History: R20260527-GO-2 switched cleanup to CompareAndDelete-on-pointer,
// which closed the Load+delete TOCTOU but left an adjacent window open — where
// executeOpt has already done `inflight := s.jobInflight(j.ID)` and is about
// to CompareAndSwap on the now-stale pointer (reachable under DeleteJob racing
// TriggerNow + 16-hex-char ID reuse). PR #1416 review (R040034-CHANGES) then
// required that residual window to stay documented rather than silently
// claimed-closed.
//
// R20260603140013-GO-2 (#1706) finally CLOSED that window with the per-jobID
// gate (job_gate.go): executeOpt holds the gate across its
// jobInflight-load→CAS pair and cleanup holds the same gate across its
// Load→running-check→CompareAndDelete, so the orphan-in-between state is no
// longer reachable. This test now pins the UPDATED documentation so a future
// edit can neither (a) drop the history anchors a reader needs to trace the
// fix, nor (b) re-assert the window is still open / that CompareAndDelete
// alone is sufficient.
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

	// History + fix anchors. The R040034-CHANGES / jobInflight(j.ID) /
	// "orphaned old gate" anchors keep the window's lineage greppable; the
	// #1706 anchor and "now closed" phrasing pin that the gate actually shut
	// the window (not a deferred follow-up).
	required := []string{
		"R040034-CHANGES",
		"jobInflight(j.ID)",
		"orphaned old gate",
		"R20260603140013-GO-2",
		"now closed",
		"per-jobID gate",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("scheduler_run.go is missing the closed-window godoc anchor %q — "+
				"the cleanupRunningJobIfIdle site must document that #1706's "+
				"per-jobID gate closed the residual split-CAS window (and keep the "+
				"history anchors so the lineage stays greppable).",
				want)
		}
	}

	// Guard against a regression that re-asserts the old "we accept this
	// window / deferred until telemetry" stance the gate replaced.
	for _, forbidden := range []string{
		"We accept this remaining window",
		"deferred until",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("scheduler_run.go still contains stale accept-the-window phrasing %q — "+
				"#1706 closed the window with the per-jobID gate; the comment must not "+
				"claim it is still open/accepted.", forbidden)
		}
	}
}
