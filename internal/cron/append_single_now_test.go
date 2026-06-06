// append_single_now_test.go: structural pin for R20260603-PERF-11.
//
// Append must capture time.Now() exactly once before the trim block so both
// skipAppendTrim's window-cutoff check and trimJobLocked share the same
// instant, eliminating a redundant vDSO call on the "do trim" path.

package cron

import (
	"os"
	"strings"
	"testing"
)

// TestAppend_SingleTimeNow_R20260603PERF11 is a structural pin that verifies
// the Append function body:
//  1. Declares `now := time.Now()` exactly once inside the trim block.
//  2. Passes `now` to skipAppendTrim (not a fresh time.Now()).
//  3. Passes `now` to trimJobLocked (not a fresh time.Now()).
//
// A future edit that reverts to two separate time.Now() calls — one inside
// skipAppendTrim's keepWindow check and one passed to trimJobLocked — would
// regress R20260603-PERF-11 and surface here rather than as silent vDSO
// overhead in production.
func TestAppend_SingleTimeNow_R20260603PERF11(t *testing.T) {
	src, err := os.ReadFile("runstore.go")
	if err != nil {
		t.Fatalf("read runstore.go: %v", err)
	}
	body := string(src)

	const fnMarker = "func (s *runStore) Append("
	idx := strings.Index(body, fnMarker)
	if idx < 0 {
		t.Fatalf("Append function not found in runstore.go")
	}
	// Restrict to the function body up to the next top-level "func ".
	rest := body[idx:]
	if next := strings.Index(rest[len(fnMarker):], "\nfunc "); next >= 0 {
		rest = rest[:len(fnMarker)+next]
	}

	// 1. exactly one `now := time.Now()` inside the trim block.
	if strings.Count(rest, "now := time.Now()") != 1 {
		t.Errorf("Append body must contain exactly one `now := time.Now()`, "+
			"found %d occurrences (R20260603-PERF-11: single vDSO call for "+
			"both skipAppendTrim and trimJobLocked)",
			strings.Count(rest, "now := time.Now()"))
	}

	// 2. skipAppendTrim must receive `now` not a fresh time.Now().
	if strings.Contains(rest, "s.skipAppendTrim(run.JobID, time.Now())") {
		t.Error("Append must not call s.skipAppendTrim(..., time.Now()); " +
			"pass the captured `now` variable instead (R20260603-PERF-11)")
	}
	if !strings.Contains(rest, "s.skipAppendTrim(run.JobID, now)") {
		t.Error("Append must call s.skipAppendTrim(run.JobID, now) to share " +
			"the single captured instant (R20260603-PERF-11)")
	}

	// 3. trimJobLocked must receive `now` not a fresh time.Now().
	if strings.Contains(rest, "s.trimJobLocked(run.JobID, time.Now())") {
		t.Error("Append must not call s.trimJobLocked(..., time.Now()); " +
			"pass the captured `now` variable instead (R20260603-PERF-11)")
	}
	if !strings.Contains(rest, "s.trimJobLocked(run.JobID, now)") {
		t.Error("Append must call s.trimJobLocked(run.JobID, now) to share " +
			"the single captured instant (R20260603-PERF-11)")
	}
}
