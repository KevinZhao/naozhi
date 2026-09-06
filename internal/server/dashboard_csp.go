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
// inline <script> (the theme bootstrap, which must run before first paint)
// is allowlisted by SHA-256 hash. The hash is computed at package init from
// the embedded dashboard.html (mirroring the login page's loginPageCSP), so
// an edit to the inline block re-derives the hash instead of silently
// breaking the page. The jsdelivr sources are pinned to the exact versioned
// files the lazy loaders inject (an /npm/ prefix is an anyone-can-publish
// namespace, i.e. an allowlist bypass); TestDashboardCSP_CDNURLsMatchBundle
// keeps them in lockstep with dashboard.js.
var dashboardCSP = buildDashboardCSP()

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

func buildDashboardCSP() string {
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		panic(fmt.Sprintf("dashboard CSP self-test: read embedded dashboard.html: %v", err))
	}
	blocks := dashInlineScriptRe.FindAllStringSubmatch(string(data), -1)
	// Exactly one inline block (the theme bootstrap) is expected. Zero means
	// the regex drifted from the markup (the CSP would then block the block);
	// more than one means someone added inline script — that needs the same
	// scrutiny as a new API and a deliberate hash here.
	if len(blocks) != 1 {
		panic(fmt.Sprintf("dashboard CSP self-test: found %d inline <script> blocks in dashboard.html, want exactly 1 (theme bootstrap)", len(blocks)))
	}
	sum := sha256.Sum256([]byte(blocks[0][1]))
	hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"

	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' " + hash + " " + cdnMermaidJS + " " + cdnKatexJS,
		"connect-src 'self'",
		// style-src unsafe-inline stays until D6 (#2559) migrates the 88
		// generated style="" attributes to classes.
		"style-src 'self' 'unsafe-inline' " + cdnKatexCSS,
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
