package server

import (
	"strings"
	"testing"
)

// TestDashboardSplitView pins the split-view docking contract: the preview
// (#fv-drawer) and 追问 (#aside-drawer) panes dock as a right-hand split on
// desktop instead of overlaying the transcript. The transcript compresses into
// the remaining width and stays visible beside the pane, and its latest
// progress is re-pinned to the bottom across the width reflow.
//
// These gates lock the structural pieces together so a future edit that drops
// any one of them (and silently reverts to the overlay or loses the
// bottom-anchor) fails CI instead of shipping:
//
//  1. HTML carries the seam element + the split CSS hooks.
//  2. JS exposes the nzSplitEnter/nzSplitExit controller.
//  3. Every drawer open/close path calls the controller, so the split is
//     actually entered and (importantly) exited.
//  4. The bottom-anchor preservation logic exists, satisfying the "keep the
//     last progress visible" requirement after the transcript narrows.
func TestDashboardSplitView_HTMLContract(t *testing.T) {
	t.Parallel()

	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// The draggable seam element must exist in the DOM.
	if !strings.Contains(html, `id="split-resizer"`) {
		t.Error("dashboard.html: #split-resizer seam element must exist — it is the " +
			"draggable divider between the transcript and the docked pane.")
	}

	// The split width variable + open-state class drive the layout. Without the
	// container padding-right the flex children never compress, so the pane
	// would overlay (regression) instead of splitting.
	if !strings.Contains(html, "--nz-split-w") {
		t.Error("dashboard.html: --nz-split-w custom property must exist — it is the " +
			"reserved right-strip width the split docks into.")
	}
	if !strings.Contains(html, ".nz-split-open .container") {
		t.Error("dashboard.html: body.nz-split-open .container must reserve padding-right " +
			"so the transcript compresses beside the pane (true split, not overlay).")
	}

	// The desktop-only gate keeps the phone full-width overlay intact.
	if !strings.Contains(html, "@media(min-width:769px)") {
		t.Error("dashboard.html: split CSS must be gated behind @media(min-width:769px) " +
			"so phones (≤768px) keep the original full-width slide-over overlay.")
	}
}

func TestDashboardSplitView_JSContract(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Controller entry points.
	if !strings.Contains(js, "window.nzSplitEnter") {
		t.Error("dashboard.js: window.nzSplitEnter must exist — it docks a drawer as a split.")
	}
	if !strings.Contains(js, "window.nzSplitExit") {
		t.Error("dashboard.js: window.nzSplitExit must exist — it undocks the split on close.")
	}

	// Both drawer open paths enter the split, and both close paths exit it.
	// Count call sites: preview open + scratch showDrawer = 2 enters;
	// preview close + scratch hideDrawer = 2 exits.
	if got := strings.Count(js, "window.nzSplitEnter()"); got < 2 {
		t.Errorf("dashboard.js: nzSplitEnter() must be called from both the preview and "+
			"追问 open paths (want ≥2 call sites, got %d) — otherwise one drawer still "+
			"overlays instead of splitting.", got)
	}
	if got := strings.Count(js, "window.nzSplitExit()"); got < 2 {
		t.Errorf("dashboard.js: nzSplitExit() must be called from both the preview and "+
			"追问 close paths (want ≥2 call sites, got %d) — otherwise closing a drawer "+
			"leaves the transcript permanently compressed.", got)
	}

	// "Keep the latest progress visible": the controller must re-pin the
	// transcript to the bottom across the width reflow. The preserveBottom
	// helper + its stickEventsBottom delegation encode that requirement.
	if !strings.Contains(js, "function preserveBottom(") {
		t.Error("dashboard.js: preserveBottom() must exist — narrowing the transcript " +
			"reflows it taller, so the newest bubbles must be re-pinned to the bottom " +
			"or the user loses sight of the latest progress.")
	}
	if !strings.Contains(js, "stickEventsBottom") {
		t.Error("dashboard.js: split controller must delegate to stickEventsBottom to " +
			"re-anchor the transcript after the width change.")
	}

	// Mobile guard: the controller must bail on a phone viewport so the
	// full-width overlay layout is preserved there.
	if !strings.Contains(js, "isMobileVp()") {
		t.Error("dashboard.js: split controller must gate on isMobileVp() so phones keep " +
			"the full-width overlay instead of an unusably-cramped split.")
	}
}
