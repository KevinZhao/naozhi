package server

import (
	"regexp"
	"strings"
	"testing"
)

// inlineStyleAttrRe matches a style="…" attribute, including the escaped-quote
// form JS template strings use.
var inlineStyleAttrRe = regexp.MustCompile(`\bstyle\s*=\s*\\?"`)

// TestDashboardBundle_NoInlineStyleAttributes is the terminal gate of #2559's
// style-src migration: neither the static markup nor any JS-emitted HTML
// carries a style="" attribute, so the CSP needs no style-src 'unsafe-inline'
// (TestDashboardCSP_StyleSrcNoUnsafeInline pins that half).
//
// What replaced them: the 28 recurring static declarations became named classes
// in css/utilities.css; the two genuinely per-element values (a node / access
// profile colour) ride a data-nz-bg attribute that nz_util applies through
// CSSOM — a property assignment, which CSP does not gate. Runtime visibility
// toggles use .nz-hidden rather than el.style.display so a class and a stale
// inline value can never disagree.
func TestDashboardBundle_NoInlineStyleAttributes(t *testing.T) {
	t.Parallel()
	files := append([]string{"dashboard.html"}, generatedOnclickBundle...)
	for _, name := range files {
		data := staticAssetBytes(name)
		if data == nil {
			t.Fatalf("%s not embedded", name)
		}
		src := string(data)
		for _, m := range inlineStyleAttrRe.FindAllStringIndex(src, -1) {
			// Prose in a comment may spell the token while explaining the rule;
			// skip a match whose line is a comment.
			lineStart := strings.LastIndex(src[:m[0]], "\n") + 1
			line := src[lineStart:]
			if i := strings.Index(line, "\n"); i >= 0 {
				line = line[:i]
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "/*") {
				continue
			}
			lineNo := 1 + strings.Count(src[:m[0]], "\n")
			t.Errorf("%s:%d: inline style attribute — use a class from css/utilities.css, "+
				"or data-nz-bg for a genuinely per-element colour (#2559; the CSP has no "+
				"style-src 'unsafe-inline')", name, lineNo)
		}
	}
}

// TestDashboardCSP_StyleSrcNoUnsafeInline is the other half: the header must
// not re-admit inline styles once the attributes are gone.
func TestDashboardCSP_StyleSrcNoUnsafeInline(t *testing.T) {
	t.Parallel()
	var styleSrc string
	for _, dir := range strings.Split(dashboardCSP, ";") {
		dir = strings.TrimSpace(dir)
		if strings.HasPrefix(dir, "style-src ") {
			styleSrc = dir
			break
		}
	}
	if styleSrc == "" {
		t.Fatalf("CSP has no style-src directive: %q", dashboardCSP)
	}
	if strings.Contains(styleSrc, "'unsafe-inline'") {
		t.Errorf("#2559: style-src re-introduced 'unsafe-inline' — the generated style attributes are "+
			"at 0 (TestDashboardBundle_NoInlineStyleAttributes), so widening back is a pure regression. got %q", styleSrc)
	}
	if !strings.Contains(styleSrc, "'self'") {
		t.Errorf("style-src must keep 'self' so the split stylesheets load: %q", styleSrc)
	}
}
