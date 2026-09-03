// static_dashboard_css_p3_test.go — #2434 light-theme / stacking / touch
// contract guards for the inline dashboard.html stylesheet (items 1-6).
//
//  1. [[feedback]] / [[reference]] memory chips route through --nz-memlink-*
//     tokens (dark literals gone) and both light blocks remap them.
//  2. Backend / access-profile chip light variant hangs off the data-theme
//     token blocks, not a bare @media (prefers-color-scheme:light) — so the
//     explicit light toggle (not just OS-follow) gets the dark-text chip.
//  3. Drawer 执行历史 .ct-head sticks below the sticky drawer header
//     (top = measured header height via CSS var), not underneath it.
//  4. Hover-revealed actions (copy / ask / code actions / rename) are always
//     visible under @media (hover:none) like .cj-run already is.
//  5. --nz-ok / --nz-warn / --nz-err referenced by .cj-placement-sandbox are
//     defined (aliases of the existing green / amber / red tokens).
//  6. role="list" containers (asset-list, files-list) emit role="listitem"
//     children from their JS renderers.
package server

import (
	"regexp"
	"strings"
	"testing"
)

func p3Static(t *testing.T, name string) string {
	t.Helper()
	var data []byte
	var err error
	switch name {
	case "dashboard.html":
		data, err = dashboardHTML.ReadFile("static/" + name)
	case "cron_view.js":
		data, err = cronViewJS.ReadFile("static/" + name)
	case "asset_browser.js":
		data, err = assetBrowserJS.ReadFile("static/" + name)
	case "files_view.js":
		data, err = filesViewJS.ReadFile("static/" + name)
	default:
		t.Fatalf("readStatic: unknown asset %q", name)
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// themeBlocks returns the :root default block plus the two light blocks
// (explicit data-theme="light" and prefers-color-scheme auto-follow).
func themeBlocks(t *testing.T, html string) (root, light, auto string) {
	t.Helper()
	root = sliceBetween(t, html, "\n:root{", "\n}")
	if !strings.Contains(root, "--nz-memlink-user-bg:") {
		t.Fatalf(":root slice does not look like the token block (missing --nz-memlink-user-bg)")
	}
	light = sliceBetween(t, html, `:root[data-theme="light"]{`, "}")
	auto = sliceBetween(t, html, `@media (prefers-color-scheme: light){`, "}\n}")
	return root, light, auto
}

func assertTokenInAllThemeBlocks(t *testing.T, html, tok string) {
	t.Helper()
	root, light, auto := themeBlocks(t, html)
	if !strings.Contains(root, tok+":") {
		t.Errorf("token %s not defined in :root", tok)
	}
	if !strings.Contains(light, tok+":") {
		t.Errorf("token %s not remapped in explicit :root[data-theme=light] block", tok)
	}
	if !strings.Contains(auto, tok+":") {
		t.Errorf("token %s not remapped in prefers-color-scheme auto-follow block", tok)
	}
}

// ruleBody returns the declaration block of the first rule whose selector
// text is exactly sel (selector immediately followed by `{`).
func ruleBody(t *testing.T, css, sel string) string {
	t.Helper()
	i := strings.Index(css, "\n"+sel+"{")
	if i < 0 {
		t.Fatalf("rule %q not found", sel)
	}
	rest := css[i+len(sel)+2:]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatalf("rule %q not terminated", sel)
	}
	return rest[:j]
}

// Item 1 — feedback / reference memory chips.
func TestDashboardHTML_MemlinkFeedbackReferenceTokens(t *testing.T) {
	t.Parallel()
	html := p3Static(t, "dashboard.html")

	for _, tok := range []string{
		"--nz-memlink-feedback-bg", "--nz-memlink-feedback-fg",
		"--nz-memlink-reference-bg", "--nz-memlink-reference-fg",
	} {
		assertTokenInAllThemeBlocks(t, html, tok)
	}
	for _, sel := range []string{".md-memlink", ".mem-pop-type"} {
		for _, kind := range []string{"feedback", "reference"} {
			body := ruleBody(t, html, sel+`[data-type="`+kind+`"]`)
			wantBg := "background:var(--nz-memlink-" + kind + "-bg)"
			wantFg := "color:var(--nz-memlink-" + kind + "-fg)"
			if !strings.Contains(body, wantBg) || !strings.Contains(body, wantFg) {
				t.Errorf("%s[data-type=%s] body %q should use %s and %s", sel, kind, body, wantBg, wantFg)
			}
		}
	}
	// The dark literals may only survive as token defaults, never as a
	// direct property value.
	for _, lit := range []string{"background:#3a2a14", "color:#f7c97a", "background:#3a1a3a", "color:#d97af7"} {
		if strings.Contains(strings.ToLower(html), lit) {
			t.Errorf("hard-coded dark literal %q still used directly — route through --nz-memlink-* token", lit)
		}
	}
}

// Item 2 — backend / access-profile chip light variant.
func TestDashboardHTML_BackendChipLightVariantViaThemeTokens(t *testing.T) {
	t.Parallel()
	html := p3Static(t, "dashboard.html")

	// No selector-level chip override living under a bare OS-preference
	// media query: it ignores the explicit data-theme toggle.
	reChipInPrefers := regexp.MustCompile(`(?s)@media \(prefers-color-scheme:\s*light\)\{[^}]*\.sc-backend-chip`)
	if reChipInPrefers.MatchString(html) {
		t.Errorf(".sc-backend-chip light variant is keyed off @media (prefers-color-scheme:light) — must follow :root[data-theme] token blocks")
	}
	for _, tok := range []string{"--nz-backend-chip-fg", "--nz-backend-chip-shadow", "--nz-backend-chip-border"} {
		assertTokenInAllThemeBlocks(t, html, tok)
	}
	body := ruleBody(t, html, ".sc-backend-chip,.sc-access-profile-chip")
	for _, want := range []string{"color:var(--nz-backend-chip-fg)", "text-shadow:var(--nz-backend-chip-shadow)", "border:1px solid var(--nz-backend-chip-border)"} {
		if !strings.Contains(body, want) {
			t.Errorf("chip rule body %q missing %q", body, want)
		}
	}
	_, light, _ := themeBlocks(t, html)
	if !strings.Contains(light, "--nz-backend-chip-shadow:none") {
		t.Errorf("light block should drop the chip text-shadow (--nz-backend-chip-shadow:none)")
	}
}

// Item 3 — drawer history head must clear the sticky drawer header.
func TestDashboardHTML_CronDrawerHistoryHeadClearsStickyHeader(t *testing.T) {
	t.Parallel()
	html := p3Static(t, "dashboard.html")
	js := p3Static(t, "cron_view.js")

	hdr := ruleBody(t, html, ".cron-drawer-header")
	if !strings.Contains(hdr, "position:sticky") || !strings.Contains(hdr, "top:0") {
		t.Fatalf(".cron-drawer-header is no longer sticky top:0 (%q) — revisit this guard", hdr)
	}
	head := ruleBody(t, html, ".cron-drawer-history .cron-timeline-panel .ct-head")
	if !strings.Contains(head, "position:sticky") {
		t.Fatalf(".ct-head in drawer is no longer sticky (%q) — revisit this guard", head)
	}
	if strings.Contains(head, "top:0") {
		t.Errorf(".ct-head sticks at top:0 — same offset as the z-index:2 drawer header, so the header covers it once scrolled")
	}
	if !strings.Contains(head, "top:var(--nz-cron-drawer-header-h") {
		t.Errorf(".ct-head top must be var(--nz-cron-drawer-header-h,…) so it sticks just below the drawer header; got %q", head)
	}
	if !strings.Contains(js, "--nz-cron-drawer-header-h") {
		t.Errorf("cron_view.js never sets --nz-cron-drawer-header-h — the drawer must publish its measured header height")
	}
}

// Item 4 — hover-only actions need a touch fallback.
func TestDashboardHTML_HoverOnlyActionsVisibleOnTouch(t *testing.T) {
	t.Parallel()
	html := p3Static(t, "dashboard.html")

	reHoverNone := regexp.MustCompile(`(?s)@media \(hover:\s*none\)\{(.*?)\n\}`)
	var joined strings.Builder
	for _, m := range reHoverNone.FindAllStringSubmatch(html, -1) {
		joined.WriteString(m[1])
		joined.WriteString("\n")
	}
	body := joined.String()
	if !strings.Contains(body, ".cj-run") {
		t.Fatalf("baseline @media (hover:none) .cj-run rule missing — regex/marker drift: %q", body)
	}
	for _, sel := range []string{".event-copy-btn.hover-only", ".event-ask-btn.hover-only", ".md-code-actions", ".btn-rename"} {
		i := strings.Index(body, sel)
		if i < 0 {
			t.Errorf("%s has no @media (hover:none) fallback — invisible on touch devices", sel)
			continue
		}
		decl := body[i:]
		if j := strings.Index(decl, "}"); j >= 0 {
			decl = decl[:j]
		}
		if !strings.Contains(decl, "opacity:1") {
			t.Errorf("%s hover:none rule does not force opacity:1 (%q)", sel, decl)
		}
	}
}

// Item 5 — status tokens referenced by the placement chip must exist.
func TestDashboardHTML_PlacementStatusTokensDefined(t *testing.T) {
	t.Parallel()
	html := p3Static(t, "dashboard.html")

	root, _, _ := themeBlocks(t, html)
	for tok, alias := range map[string]string{
		"--nz-ok":   "var(--nz-green)",
		"--nz-warn": "var(--nz-amber)",
		"--nz-err":  "var(--nz-red)",
	} {
		if !strings.Contains(root, tok+":"+alias) {
			t.Errorf("%s not defined in :root as alias %s (placement chip falls back to hard-coded hex that ignores the theme)", tok, alias)
		}
	}
	body := ruleBody(t, html, ".cj-placement-sandbox")
	if !strings.Contains(body, "var(--nz-ok") {
		t.Errorf(".cj-placement-sandbox no longer references --nz-ok (%q) — revisit this guard", body)
	}
}

// Item 6 — role="list" containers must have role="listitem" children.
func TestStaticJS_ListContainersEmitListitems(t *testing.T) {
	t.Parallel()
	html := p3Static(t, "dashboard.html")
	for _, c := range []string{`id="asset-sidebar-list" role="list"`, `id="files-list" role="list"`} {
		if !strings.Contains(html, c) {
			t.Fatalf("container %q not found — revisit this guard", c)
		}
	}
	cases := []struct{ file, marker string }{
		{"asset_browser.js", `'<div class="asset-row'`},
		{"files_view.js", `'<div class="files-row files-up"`},
		{"files_view.js", `'<div class="' + cls + '"`},
	}
	for _, c := range cases {
		src := p3Static(t, c.file)
		found := false
		for _, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, c.marker) {
				continue
			}
			found = true
			if !strings.Contains(line, `role="listitem"`) {
				t.Errorf("%s: row emitted at %q lacks role=\"listitem\" (parent is role=\"list\")", c.file, strings.TrimSpace(line))
			}
		}
		if !found {
			t.Errorf("%s: marker %q not found — renderer changed, update this guard", c.file, c.marker)
		}
	}
}
