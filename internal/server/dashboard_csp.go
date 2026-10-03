package server

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// dashboardCSP is the Content-Security-Policy served with /dashboard.
//
// #1980: script-src carries no 'unsafe-inline' — the dashboard bundle wires
// every handler through the nz.actions data-action delegation, and the only
// inline scripts (the theme bootstrap, which must run before first paint, and
// the generated import map and entry loaders) are allowlisted by SHA-256 hash,
// computed from the page as served, so an edit re-derives the hash instead of
// silently breaking the page. The jsdelivr sources are pinned to the exact versioned files the
// lazy loaders inject (an /npm/ prefix is an anyone-can-publish namespace,
// i.e. an allowlist bypass); TestDashboardCSP_CDNURLsMatchBundle keeps them in
// lockstep with dashboard.js.
var dashboardCSP = buildDashboardCSP(staticAssets["dashboard.html"].bytes)

// cdn URLs the dashboard's lazy loaders inject (SRI-pinned in dashboard.js).
const (
	cdnMermaidJS = "https://cdn.jsdelivr.net/npm/mermaid@11.14.0/dist/mermaid.min.js"
	cdnKatexJS   = "https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/katex.min.js"
	cdnKatexCSS  = "https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/katex.min.css"
	// Fonts are referenced from within the KaTeX CSS; a path prefix is the
	// tightest expressible source (the font set varies by glyph usage).
	cdnKatexFonts = "https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/fonts/"
)

// dashInlineScriptRe matches inline <script> blocks WITHOUT a src attribute —
// external tags (<script defer src=…>) have empty bodies and must not
// contribute an empty-string hash to the allowlist.
var dashInlineScriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// dashImportMapRe matches the import map renderDashboardHTML generates.
var dashImportMapRe = regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`)

func cspHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// buildDashboardCSP derives the policy for page. The raw page carries no
// import map or entry loaders, so it yields the policy without those hashes,
// which is the one the e2e mock serves next to the raw page.
func buildDashboardCSP(page []byte) string {
	if page == nil {
		panic("dashboard CSP self-test: dashboard.html is not embedded")
	}
	blocks := dashInlineScriptRe.FindAllStringSubmatch(string(page), -1)
	// Exactly one inline block (the theme bootstrap) is expected. Zero means
	// the regex drifted from the markup (the CSP would then block the block);
	// more than one means someone added inline script — that needs the same
	// scrutiny as a new API and a deliberate hash here.
	if len(blocks) != 1 {
		panic(fmt.Sprintf("dashboard CSP self-test: found %d inline <script> blocks in dashboard.html, want exactly 1 (theme bootstrap)", len(blocks)))
	}
	hashes := cspHash(blocks[0][1])
	maps := dashImportMapRe.FindAllStringSubmatch(string(page), -1)
	if len(maps) > 1 {
		panic(fmt.Sprintf("dashboard CSP self-test: found %d import maps in dashboard.html, want at most 1", len(maps)))
	}
	for _, m := range maps {
		hashes += " " + cspHash(m[1])
	}
	for _, m := range moduleLoaderRe.FindAllStringSubmatch(string(page), -1) {
		hashes += " " + cspHash(m[1])
	}

	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' " + hashes + " " + cdnMermaidJS + " " + cdnKatexJS,
		"connect-src 'self'",
		// #2559 D6-3 dropped the last generated style="" attribute, so inline
		// styles are no longer needed. KaTeX's stylesheet is the one external
		// source (SRI-pinned where it is injected).
		"style-src 'self' " + cdnKatexCSS,
		"font-src 'self' " + cdnKatexFonts,
		"img-src 'self' data: blob:",
		"frame-src 'self' blob:",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"require-sri-for script style font",
	}, "; ")
}
