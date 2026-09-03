// static_effort_tag_contract_test.go — wiring pins for the header effort tag.
//
// dashboard.js has no JS unit runner, so these Go source-greps guard the
// invariants a silent refactor could break without any test failing:
//  1. the tag is repainted from the 5 s poll, not only from renderMainShell.
//     renderMainShell runs on select / rename / create only — never on turn
//     completion — so an inline build there would leave the tier stale until
//     the operator switched sessions and back.
//  2. the tier string passes through esc/escAttr. It is supplied by the kiro
//     process via _kiro.dev/metadata, i.e. a separate trust boundary, exactly
//     like the git branch names guarded in static_git_chip_contract_test.go.
//  3. an unrecognised tier still renders. kiro owns this vocabulary; dropping
//     an unknown tier would report the session as having no effort at all.
//
// Per the #388 guidance in static_ux_contract_test.go this file stays small
// and asserts wiring, not DOM shape. docs/rfc/kiro-effort-visibility.md
package server

import (
	"strings"
	"testing"
)

func TestDashboardJS_EffortTagWiring(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// Builder + setter pair, mirroring gitChipHtml / setHeaderGitChip.
		`function effortTagHtml(`,
		`function setHeaderEffortChip(`,
		// Mount node built empty by renderMainShell.
		`id="header-effort"`,
		`getElementById('header-effort')`,
		// Reads the tier off the snapshot the REST poll already delivers —
		// no dedicated endpoint.
		`effortTagHtml(effort)`,
		// Both sources of the tier: the fresh poll rows (pre-short-circuit) and
		// the cached sessionsData map (header rebuild).
		`row.effort`,
		`}).effort`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing effort-tag wiring: %q", want)
		}
	}

	// The mount node must exist exactly once — a duplicate (e.g. copied into
	// previewDiscovered's hand-rolled header) would make getElementById pick
	// whichever comes first and let a discovered session inherit a managed
	// session's tier.
	if n := strings.Count(js, `id="header-effort"`); n != 1 {
		t.Errorf(`id="header-effort" appears %d times, want exactly 1 (duplicate mount = wrong session's tier)`, n)
	}

	// The repaint must be wired at BOTH sites. Assert each one's surrounding
	// context rather than a global occurrence count: the bare token also shows
	// up in prose comments, so a count would silently track comment edits and
	// point a future failure at the wrong thing.
	//
	// The fetchSessions site is the load-bearing half AND is position-sensitive
	// — it must precede the `version === lastVersion` short-circuit, because a
	// tier change does not advance stats.version. An earlier revision of this
	// code sat after the short-circuit and never fired at all.
	const pollSite = `if (selectedKey) setHeaderEffortChip(data.sessions);`
	if !strings.Contains(js, pollSite) {
		t.Errorf("fetchSessions must repaint the effort tag: missing %q", pollSite)
	}
	shortCircuit := strings.Index(js, `if (wsConnected && version === lastVersion && version > 0`)
	poll := strings.Index(js, pollSite)
	switch {
	case shortCircuit < 0:
		t.Error("could not locate fetchSessions version short-circuit; " +
			"re-check that the effort repaint still precedes it")
	case poll < 0:
		// already reported above
	case poll > shortCircuit:
		t.Error("setHeaderEffortChip(data.sessions) must come BEFORE the " +
			"version short-circuit in fetchSessions — a turn boundary does not " +
			"advance stats.version, so a repaint after the early return never " +
			"runs and the tier stays stale until the operator re-selects")
	}
	// renderMainShell's own repaint (no argument — reads cached sessionsData)
	// sits beside repaintGitChip for the same reason: the rebuild emptied the
	// mount node.
	if !strings.Contains(js, "repaintGitChip();\n  // Same rationale") {
		t.Error("renderMainShell must repaint the effort tag next to repaintGitChip()")
	}
}

// TestDashboardJS_EffortTagEscapesTier pins that the tier string is escaped on
// both the text and attribute paths. It arrives from the kiro process over
// JSON-RPC, so an unescaped interpolation into the header would be an
// injection vector reachable by whatever that process reports.
func TestDashboardJS_EffortTagEscapesTier(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		`esc(raw)`,     // visible tag text
		`escAttr(tip)`, // title attribute
	} {
		if !strings.Contains(js, want) {
			t.Errorf("effortTagHtml must escape dynamic values: missing %q", want)
		}
	}
}

// TestDashboardJS_EffortLabelsCoverKnownTiers pins the tooltip gloss table
// against kiro 2.16.0's tier vocabulary (`kiro-cli acp --help`:
// "low, medium, high, xhigh, max"). A tier missing from the table still
// renders — only its Chinese gloss is lost — so this guards documentation
// completeness, not correctness.
func TestDashboardJS_EffortLabelsCoverKnownTiers(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, "const EFFORT_LABELS = {") {
		t.Fatal("dashboard.js missing EFFORT_LABELS table")
	}
	for _, tier := range []string{"low:", "medium:", "high:", "xhigh:", "max:"} {
		if !strings.Contains(js, tier) {
			t.Errorf("EFFORT_LABELS missing kiro tier %q", strings.TrimSuffix(tier, ":"))
		}
	}

	// The two top tiers drive the .effort-hot emphasis. Pin the condition so a
	// refactor cannot silently drop the emphasis or extend it to every tier.
	if !strings.Contains(js, `raw === 'max' || raw === 'xhigh'`) {
		t.Error("effortTagHtml must emphasise exactly max/xhigh via .effort-hot")
	}
}

// TestDashboardHTML_EffortTagStyled pins that the tag has CSS backing it. A
// mount node with no rule would render the tier at inherited size and colour,
// competing with the primary cli/model label instead of sitting beside it.
func TestDashboardHTML_EffortTagStyled(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	for _, want := range []string{
		`.main-header .detail-effort{`,
		// Collapses when the backend reports no tier, so the header keeps its
		// spacing — same treatment as .detail-runstats / .detail-git.
		`.main-header .detail-effort:empty{display:none}`,
		`.effort-hot{`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard.html missing effort-tag style: %q", want)
		}
	}
}
