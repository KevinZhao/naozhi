// static_light_theme_parity_test.go — light-theme colour parity guard (R20260608).
//
// #453 shipped light theme as a "minimal scaffold": many component rules still
// hard-coded GitHub-Dark hex (near-white text, dark-blue fills) that never got
// a light override, so on a white background they fell to <2:1 contrast — chat
// bubbles, the running banner, code text, IM-origin chips and the count badge
// were the worst offenders. The R20260608 sweep tokenised those literals
// (default = the original dark value) and remapped them in the two light blocks
// (explicit data-theme="light" + prefers-color-scheme auto-follow).
//
// This guard pins two invariants so the fix can't silently regress:
//  1. The offending hard-coded literals are gone from the rule bodies — a fresh
//     `color:#a5d6ff` / `background:#1a2332` etc. trips the test, steering new
//     code to the token.
//  2. Every parity token is overridden in BOTH light blocks (a token defined
//     once but only remapped in the explicit block would leave OS-follow users
//     on the dark default).
package server

import (
	"regexp"
	"strings"
	"testing"
)

// Parity tokens introduced by the R20260608 sweep. Each MUST be remapped in
// both light blocks, else one of the two light entry points keeps the dark
// default.
var lightParityTokens = []string{
	"--nz-event-agent-fg",
	"--nz-event-user-fg",
	// --nz-banner-bg retired (ui-polish-light-theme D5): the running banner
	// dropped its tinted strip for quiet chrome, leaving the token unused.
	"--nz-code-fg",
	"--nz-chip-info-fg",
	"--nz-chip-info-bg",
	"--nz-overlay-pill-bg",
	"--nz-badge-info",
	"--nz-badge-info-text",
	// R20260610-UI-1: history-popover project label gold. Was a bare #d4a017
	// with no light override; tokenised + remapped to a deeper gold for light.
	"--nz-project-label-fg",
	// 2026-09 light hard-coded fg sweep: IM-origin chip text (slack / discord /
	// weixin were pastel dark-theme literals, <2:1 on white) and the favourite
	// star (gold #e3b341 ~1.7:1 on white). Both light blocks must remap.
	"--nz-origin-slack-fg",
	"--nz-origin-discord-fg",
	"--nz-origin-weixin-fg",
	"--nz-star-fg",
	"--nz-star-fg-hover",
}

// Literals that the sweep removed from rule bodies. They legitimately remain as
// token DEFAULT values inside :root, so the guard only forbids them when used
// directly as a property value (`color:#hex` / `background:#hex`), not in a
// `--token:#hex;` definition.
var forbiddenLightLiterals = []string{
	"#a5d6ff", // near-white blue prose / chip text
	"#e6edf3", // near-white bubble + code text
	"#1a2332", // dark-blue running banner
	"#1c2128", // dark voice card
	"#1f2937", // alien dark-blue hover / active fill
	"#d4a017", // history-popover project label gold (no light override) — R20260610-UI-1
	// 2026-09 sweep: GitHub-Dark pastels that only read on a dark surface.
	"#7ee787", // doctor-feat-on / doctor-status-ok / tc-status-completed / event.result → --nz-green
	"#79c0ff", // tc-status-in_progress / cmd-palette mark → --nz-accent
	"#ff7b72", // tc-status-failed → --nz-red
	"#d8b4fe", // sc-origin.kind-slack → --nz-origin-slack-fg
	"#b4c0ff", // sc-origin.kind-discord → --nz-origin-discord-fg
	"#a7f3d0", // sc-origin.kind-weixin → --nz-origin-weixin-fg
	"#e3b341", // favourite star (sidebar + palette) → --nz-star-fg
	"#f7c744", // favourite star hover → --nz-star-fg-hover
}

var reColorOrBgLiteral = regexp.MustCompile(`(?:color|background(?:-color)?):#[0-9a-fA-F]{3,8}`)

func TestDashboardHTML_LightThemeParity(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// (1) No forbidden literal appears as a direct property value. We scan
	// every `color:#..`/`background:#..` occurrence; a hit on a forbidden hex
	// means a rule body re-introduced the dark literal instead of the token.
	for _, m := range reColorOrBgLiteral.FindAllString(html, -1) {
		for _, lit := range forbiddenLightLiterals {
			if strings.HasSuffix(strings.ToLower(m), strings.ToLower(lit)) {
				t.Errorf("rule body uses dark literal %q directly (%q) — route it through a --nz-* parity token so light theme can remap it", lit, m)
			}
		}
	}

	// (2) Each parity token is overridden in both light blocks. Locate the
	// explicit block and the prefers-color-scheme auto block, then assert each
	// token name appears as an assignment (`--token:`) inside each.
	explicit := sliceBetween(t, html, `:root[data-theme="light"]{`, "}")
	autoFollow := sliceBetween(t, html, `@media (prefers-color-scheme: light){`, "}\n}")

	for _, tok := range lightParityTokens {
		if !strings.Contains(explicit, tok+":") {
			t.Errorf("parity token %s not remapped in explicit :root[data-theme=light] block", tok)
		}
		if !strings.Contains(autoFollow, tok+":") {
			t.Errorf("parity token %s not remapped in prefers-color-scheme auto-follow block", tok)
		}
	}
}

// sliceBetween returns the substring after the first occurrence of start up to
// the first occurrence of end after it. Fails the test if either marker is
// absent so a structural rename of the light blocks surfaces loudly.
func sliceBetween(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found in dashboard.html — light-theme block was renamed/removed", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("end marker %q not found after %q", end, start)
	}
	return rest[:j]
}
