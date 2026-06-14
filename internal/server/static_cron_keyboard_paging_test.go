package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronKeyboardNavPaging pins R20260614-LOGIC-8 (#2090):
// navigateExpandedRun must trigger pagination when the user arrows DOWN past
// the last loaded run on a multi-page timeline, instead of silently no-oping
// at the bounds guard. The fix loads the next page (when !st.done) and expands
// the target once it arrives, via a cronTimelineLoadMore onDone callback.
//
// Per-file assertion on the embedded source (the cron_view.js split lesson:
// no union snapshot). These fragments are the load-bearing pieces of the fix;
// if a refactor drops the paging branch the keyboard would regress to the
// silent dead-end this issue reported.
func TestDashboardJS_CronKeyboardNavPaging(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	js := string(data)

	for _, frag := range []string{
		// navigate triggers a page load when stepping past the loaded tail.
		"if (nextIdx >= st.runs.length) {",
		"if (direction !== 'next' || st.done || st.loading) return",
		"cronTimelineLoadMore(jobId, () => {",
		// after the page lands, navigation re-resolves and expands the next run.
		"const i2 = st2.runs.findIndex(r => r && r.run_id === cronExpandedRunId.runId);",
		"if (i2 < 0 || i2 + 1 >= st2.runs.length) return",
		// loadMore exposes a post-success hook, fired only on a real load.
		"function cronTimelineLoadMore(jobId, onDone) {",
		"if (loaded && typeof onDone === 'function') {",
	} {
		if !strings.Contains(js, frag) {
			t.Errorf("cron_view.js missing keyboard-paging fragment %q", frag)
		}
	}

	// Guard against a regression to the old combined bounds guard that made
	// the DOWN-past-end case a silent no-op.
	if strings.Contains(js, "if (nextIdx < 0 || nextIdx >= st.runs.length) return;") {
		t.Error("cron_view.js still has the combined bounds guard that silently no-ops ↓ past the last loaded run (#2090)")
	}
}
