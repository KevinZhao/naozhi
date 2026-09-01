package server

import (
	"strings"
	"testing"
)

// TestUpdateChipMarkup locks the sidebar-header chip's markup.
//
// The `hidden` default is the load-bearing part: without it the chip paints on
// every cold start before the first /api/system/update response lands, telling
// operators an update exists when none does.
func TestUpdateChipMarkup(t *testing.T) {
	html := string(staticAssetBytes("dashboard.html"))
	if html == "" {
		t.Fatal("dashboard.html asset empty")
	}

	idAt := strings.Index(html, `id="btn-update"`)
	if idAt < 0 {
		t.Fatal(`#btn-update not found in dashboard.html`)
	}
	// Expand backwards to the opening tag: attributes like class= sit before
	// the id, so slicing forward from the id would miss them.
	start := strings.LastIndex(html[:idAt], "<button")
	if start < 0 {
		t.Fatal("could not find the opening <button tag for #btn-update")
	}
	end := strings.Index(html[start:], "</button>")
	if end < 0 {
		t.Fatal("could not find the end of the #btn-update element")
	}
	btn := html[start : start+end]

	if !strings.Contains(btn, "hidden") {
		t.Error(`#btn-update must be ` + "`hidden`" + ` by default; otherwise it flashes "update available" on every cold start before the first status poll resolves`)
	}
	if !strings.Contains(btn, `id="update-tag"`) {
		t.Error("#btn-update must contain the #update-tag span the version string is written into")
	}
	if !strings.Contains(btn, "hdr-btn-update") {
		t.Error("#btn-update must carry the hdr-btn-update class the CSS targets")
	}
	// Both title and aria-label are populated by JS per action; the attributes
	// must exist so the assignment has something to overwrite and screen
	// readers never see a bare icon button.
	if !strings.Contains(btn, "aria-label") {
		t.Error("#btn-update must declare aria-label (JS fills it per action)")
	}
}

// TestUpdateChipStylesUseExistingTokens keeps the chip inside the design
// system. Every colour it uses must be an existing --nz-* token: hard-coded
// hex in a new component is how a theme drifts, and the light-theme variants
// only track the tokens.
func TestUpdateChipStylesUseExistingTokens(t *testing.T) {
	html := string(staticAssetBytes("dashboard.html"))
	// Collect exactly the chip's own rules rather than guessing a byte range:
	// a range that overshoots picks up neighbouring components' colours and
	// reports them as this component's violations.
	var rules []string
	for _, line := range strings.Split(html, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ".hdr-btn-update") {
			rules = append(rules, trimmed)
		}
	}
	if len(rules) == 0 {
		t.Fatal(".hdr-btn-update CSS rules not found")
	}
	block := strings.Join(rules, "\n")

	for _, marker := range []string{"#fff", "#000", "rgb("} {
		if strings.Contains(block, marker) {
			t.Errorf("hdr-btn-update CSS contains the literal colour %q; use an existing --nz-* token so light theme tracks it:\n%s", marker, block)
		}
	}
	for _, tok := range []string{"var(--nz-accent)", "var(--nz-red)"} {
		if !strings.Contains(block, tok) {
			t.Errorf("expected the chip to reuse %s", tok)
		}
	}
}

// TestUpdateChipJSContract guards the browser-side rules that the RFC identifies
// as correctness-critical rather than cosmetic.
func TestUpdateChipJSContract(t *testing.T) {
	js := string(staticAssetBytes("dashboard.js"))
	if js == "" {
		t.Fatal("dashboard.js asset empty")
	}

	if !strings.Contains(js, "'/api/system/update'") {
		t.Error("dashboard.js must fetch /api/system/update")
	}

	// The browser must branch on the server-computed `action` and must NOT
	// reimplement version comparison — two copies of that logic drift, and the
	// failure mode (offering "install" for an already-staged binary) destroys
	// the rollback backup. See docs/rfc/dashboard-update-notice.md §1.3.
	if !strings.Contains(js, "st.action === 'restart'") {
		t.Error("the chip must special-case action === 'restart' (staged binary: restart only, no re-download)")
	}

	// Both action paths must produce DIFFERENT operator instructions. Telling
	// someone to re-run `naozhi upgrade` when the binary is already staged is
	// exactly what overwrites the backup.
	if !strings.Contains(js, "naozhi upgrade") {
		t.Error("the install path must surface the `naozhi upgrade` command")
	}
	if !strings.Contains(js, "kickstart") || !strings.Contains(js, "systemctl restart naozhi") {
		t.Error("the restart path must surface a restart command for both macOS (launchctl kickstart) and Linux (systemctl restart)")
	}

	// Multi-node deployments: the endpoint only ever acts on the process the
	// dashboard is connected to, so the wording has to say so (RFC NG2).
	if !strings.Contains(js, "本节点") {
		t.Error("update copy must say 本节点 so multi-node operators do not read it as a fleet-wide upgrade")
	}

	// Poll cadence must stay slow — this is 6h-cadence server state, and the
	// endpoint is not something to add to the per-second loops.
	if !strings.Contains(js, "UPDATE_POLL_MS = 60000") {
		t.Error("expected a 60s poll interval for the update status; a faster cadence adds load for state that changes on a 6h server cadence")
	}
}
