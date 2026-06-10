package server

import (
	"regexp"
	"strings"
	"testing"
)

// TestCronViewJS_TimelineRunsCap pins the R20260610-CR-007 (#1998) fix:
// cronTimelineRefreshHead merges the WS-triggered head-10 fetch with the
// existing st.runs, and before the fix the merged array was written back
// with no size limit — deep loadMore paging followed by repeated
// cron_run_ended refreshes grew st.runs without bound, paying an
// O(n log n) sort + full re-render per WS event over the whole array.
//
// Contract:
//  1. CRON_TIMELINE_MAX_RUNS exists as a named constant (cap is tunable
//     in one place, mirrors CRON_TIMELINE_MAX_ENTRIES style).
//  2. The refreshHead merge truncates to that cap.
//  3. Truncation re-arms pagination (st.done = false + cursor moved to
//     the new oldest entry) so 加载更多 can still page past the cut.
func TestCronViewJS_TimelineRunsCap(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	// 1. Named cap constant.
	if !regexp.MustCompile(`const CRON_TIMELINE_MAX_RUNS = \d+`).MatchString(js) {
		t.Error("cron_view.js: CRON_TIMELINE_MAX_RUNS constant must exist (#1998)")
	}

	// Scope the remaining assertions to the cronTimelineRefreshHead body so
	// they pin the merge path specifically, not some unrelated use.
	fnIdx := strings.Index(js, "async function cronTimelineRefreshHead(jobId)")
	if fnIdx < 0 {
		t.Fatal("cron_view.js: cronTimelineRefreshHead not found")
	}
	end := fnIdx + 4000
	if end > len(js) {
		end = len(js)
	}
	body := js[fnIdx:end]

	// 2. Merge truncates to the cap.
	if !strings.Contains(body, "merged.length > CRON_TIMELINE_MAX_RUNS") {
		t.Error("cronTimelineRefreshHead: must check merged.length against CRON_TIMELINE_MAX_RUNS after the merge (#1998)")
	}
	if !strings.Contains(body, "merged.length = CRON_TIMELINE_MAX_RUNS") {
		t.Error("cronTimelineRefreshHead: must truncate merged to CRON_TIMELINE_MAX_RUNS (#1998)")
	}

	// 3. Truncation re-arms pagination so loadMore can continue past the cut.
	capIdx := strings.Index(body, "merged.length = CRON_TIMELINE_MAX_RUNS")
	if capIdx >= 0 {
		afterCap := body[capIdx:]
		if !strings.Contains(afterCap, "st.done = false") {
			t.Error("cronTimelineRefreshHead: truncation must reset st.done = false so loadMore can re-fetch the dropped tail (#1998)")
		}
		if !strings.Contains(afterCap, "st.nextBefore") {
			t.Error("cronTimelineRefreshHead: truncation must move st.nextBefore to the new oldest entry (#1998)")
		}
	}
}
