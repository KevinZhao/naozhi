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
	// The open-state class reserves the right strip on .container. The selector
	// is scoped with :not(.nz-view-*) so the split applies only to the chat
	// view (the drawers don't exist in assets/cron/settings) — assert both the
	// class+container coupling and that the chat-only scoping guard is present.
	if !strings.Contains(html, "body.nz-split-open") || !strings.Contains(html, ".container{padding-right:var(--nz-split-w)}") {
		t.Error("dashboard.html: body.nz-split-open … .container must reserve padding-right " +
			"so the transcript compresses beside the pane (true split, not overlay).")
	}
	if !strings.Contains(html, ":not(.nz-view-cron)") {
		t.Error("dashboard.html: split selectors must be scoped with :not(.nz-view-*) so " +
			"switching to assets/cron/settings doesn't reserve the split strip in a " +
			"drawer-less view.")
	}

	// The desktop-only gate keeps the phone full-width overlay intact.
	if !strings.Contains(html, "@media(min-width:769px)") {
		t.Error("dashboard.html: split CSS must be gated behind @media(min-width:769px) " +
			"so phones (≤768px) keep the original full-width slide-over overlay.")
	}

	// Z-order: when both preview and 追问 are docked, the one opened last carries
	// .nz-split-front and must paint above the other (higher z-index than the
	// base 200). Without this rule, two same-z drawers fully overlap with no
	// stacking and the last-opened pane can sit behind.
	if !strings.Contains(html, ".nz-split-front") || !strings.Contains(html, "z-index:202") {
		t.Error("dashboard.html: .nz-split-front must lift the last-opened drawer above " +
			"the other (z-index 202) so the most recently shown pane stacks on top.")
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

	// View-router teardown: leaving the chat view must close the docked drawers,
	// otherwise the position:fixed pane floats over assets/cron/settings and the
	// split padding stays reserved. setActivityView must reach both close paths.
	if !strings.Contains(js, "window.__closeScratchDrawer") {
		t.Error("dashboard.js: a scratch-close global must exist so setActivityView can " +
			"tear the 追问 drawer down when leaving chat.")
	}
	if !strings.Contains(js, "prev === 'chat' && view !== 'chat'") {
		t.Error("dashboard.js: setActivityView must close the preview/追问 drawers when " +
			"leaving the chat view (drawers are position:fixed and would float over the " +
			"target view, leaving the split padding reserved).")
	}

	// Resize re-clamp: clampW otherwise only runs on drag/dblclick/cold-load, so
	// shrinking the viewport with the split open could leave --nz-split-w wider
	// than the viewport and crush the transcript. A resize listener must re-clamp.
	if !strings.Contains(js, "addEventListener('resize'") {
		t.Error("dashboard.js: split controller must re-clamp --nz-split-w on window " +
			"resize so narrowing the viewport can't reserve more than it fits.")
	}

	// Half-width default: the pane defaults to half the dashboard width on PC
	// (innerWidth/2), not a fixed pixel size. splitDefaultW encodes that, and
	// hasCustomW gates whether a manual drag opts out of auto-tracking.
	if !strings.Contains(js, "function splitDefaultW(") || !strings.Contains(js, "window.innerWidth / 2") {
		t.Error("dashboard.js: splitDefaultW() must default the pane to half the dashboard " +
			"width (window.innerWidth / 2) on PC.")
	}
	if !strings.Contains(js, "hasCustomW") {
		t.Error("dashboard.js: a hasCustomW flag must gate half-width auto-tracking so a " +
			"manual drag is honoured while the default follows the viewport at one half.")
	}

	// Z-order controller: the last-opened drawer is brought to the front, and
	// both open paths invoke it (one call per open path).
	if !strings.Contains(js, "window.nzSplitBringToFront = function") {
		t.Error("dashboard.js: window.nzSplitBringToFront must be defined to stack the " +
			"last-opened drawer above the other.")
	}
	if got := strings.Count(js, "window.nzSplitBringToFront("); got < 2 {
		t.Errorf("dashboard.js: nzSplitBringToFront must be called from both open paths "+
			"(preview + 追问; want ≥2 invocations, got %d) so whichever opens last stacks "+
			"on top.", got)
	}
}
