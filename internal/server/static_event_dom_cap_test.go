package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_LiveEventDOMCap pins the live-DOM bound for UX3 (#398):
// the live-push path (appendEvents) used to call insertAdjacentHTML('beforeend')
// once per event with no ceiling, so a long-running session that streams
// thousands of events grew #events-scroll without limit until the tab OOMed.
// The historical-load path is already paginated (INITIAL_HISTORY_LIMIT +
// "load earlier"); these gates assert the live half carries the same budget:
//
//  1. A MAX_LIVE_DOM_EVENTS ceiling constant exists.
//  2. A trimEventsScroll helper exists to evict oldest top nodes.
//  3. appendEvents actually calls the trim helper, so the cap is enforced on
//     the live path (not just declared).
//
// Without these, #398's OOM symptom regresses silently — the bug is invisible
// until a power user's multi-hour session blows up the browser.
func TestDashboardJS_LiveEventDOMCap(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, "const MAX_LIVE_DOM_EVENTS") {
		t.Error("dashboard.js: MAX_LIVE_DOM_EVENTS ceiling constant must exist (#398): " +
			"the live-append DOM bound is the live-side counterpart to INITIAL_HISTORY_LIMIT.")
	}

	if !strings.Contains(js, "function trimEventsScroll(") {
		t.Error("dashboard.js: trimEventsScroll(el) must exist (#398): it evicts the " +
			"oldest top event bubbles once the live DOM exceeds MAX_LIVE_DOM_EVENTS.")
	}

	if !strings.Contains(js, "trimEventsScroll(el)") {
		t.Error("dashboard.js: appendEvents must call trimEventsScroll(el) (#398); " +
			"declaring the cap without enforcing it on the live-push path leaves the " +
			"unbounded-DOM OOM unfixed.")
	}

	// The cron-live container has the sibling bug: onCronLiveEvent caps the
	// data model at CRON_LIVE_MAX_EVENTS but appendEventsToContainer only
	// appended, so its DOM grew unbounded. Pin that the container append
	// path now trims against the same ceiling. appendEventsToContainer moved
	// to cron_view.js with the cron extraction (PR-1), so assert it there.
	cronData, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	if !strings.Contains(string(cronData), "bubbles > CRON_LIVE_MAX_EVENTS") {
		t.Error("cron_view.js: appendEventsToContainer must trim oldest .event nodes " +
			"against CRON_LIVE_MAX_EVENTS (#398): the data model already shifts at the " +
			"cap, so the DOM must too or a long cron run OOMs the tab.")
	}
}
