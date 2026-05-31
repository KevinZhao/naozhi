package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_FencedPathList_Contract pins the fix for path-list fences:
// an AI reply that lists generated files inside a ```-fence (one path per
// line — including non-ASCII paths like `数学/专题/foo.html`) must render as
// clickable rows so the inline file-ref scanner can attach preview/download
// buttons. Before the fix those paths lived inside <pre><code>, which
// scanEventForFileRefs deliberately skips, so they never got buttons.
//
// We pin the JS contract by windowing on the helper + its render branch
// rather than asserting on a live DOM (dashboard.js is exercised as an
// embedded asset; there is no JS test runner in CI). The behavioural matrix
// (Chinese paths matched, mixed/real-code blocks rejected, single path left
// verbatim) is verified out-of-band via the regex in isFileRefCandidate,
// which this helper reuses unchanged.
func TestDashboardJS_FencedPathList_Contract(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. The helper must exist.
	fnIdx := strings.Index(js, "function fencedPathList(code)")
	if fnIdx < 0 {
		t.Fatal("fencedPathList helper not found in dashboard.js — the path-list fence fix is missing")
	}
	rest := js[fnIdx:]
	end := strings.Index(rest[1:], "\nfunction ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// 2. It must reuse the candidate matcher (so Unicode paths inherit the
	// same Unicode-safe regex). A single path line is now accepted (a lone
	// generated file inside a ``` fence is a very common AI shape); the
	// double gate (isFileRefCandidate + FILE_REF_HAS_EXT) plus the server-side
	// existence check keep a single-line list from misrendering prose.
	if !strings.Contains(body, "isFileRefCandidate(") {
		t.Error("fencedPathList must reuse isFileRefCandidate so non-ASCII paths are matched by the same Unicode-safe regex")
	}
	if strings.Contains(body, "paths.length < 2") {
		t.Error("fencedPathList must NOT require >= 2 path lines — that left every single-file fence button-less; a lone path with an extension is a valid one-item list")
	}
	if !strings.Contains(body, "paths.length < 1") {
		t.Error("fencedPathList must still bail on an empty fence (paths.length < 1)")
	}
	// The extension gate is what keeps dependency/module/REST/fraction lists
	// (every line matches the slash-path regex but carries no `.ext`) from
	// hijacking a no-language code block. Pin it so a refactor can't drop it.
	if !strings.Contains(body, "FILE_REF_HAS_EXT") {
		t.Error("fencedPathList must require each basename to carry a file extension (FILE_REF_HAS_EXT) — otherwise dependency lists like @angular/core or github.com/gin-gonic/gin get misrendered as path lists")
	}
	// Any non-path line must abort the whole block (returns null), keeping
	// real code blocks on the verbatim <pre> path. Pin the early-return guard.
	if !strings.Contains(body, "return null") {
		t.Error("fencedPathList must bail (return null) on the first non-path line so real code blocks are never reinterpreted as path lists")
	}

	// 3. The render branch must be gated on a language-less fence and must
	// emit a .md-pathlist wrap (the class _codeBlockInfo + CSS key off).
	if !strings.Contains(js, "lang === '' ? fencedPathList(code)") {
		t.Error("path-list rendering must be gated on a language-less fence (lang === '') so ```go etc. keep verbatim rendering")
	}
	if !strings.Contains(js, "md-code-wrap md-pathlist") {
		t.Error("path-list fence must render a .md-pathlist wrap so the file-ref scanner and copy handler can find the rows")
	}
	if !strings.Contains(js, `class="md-pathline"`) {
		t.Error("each path line must be wrapped in .md-pathline with its own <code> (not <pre>) so scanEventForFileRefs attaches preview/download buttons")
	}

	// 4. The rows must NOT be wrapped in <pre>: scanEventForFileRefs skips
	// any <code> with a <pre> ancestor, so a <pre>-wrapped path list would
	// regress to the original (button-less) behaviour.
	pathlistIdx := strings.Index(js, "md-code-wrap md-pathlist")
	if pathlistIdx >= 0 {
		// Inspect the render expression up to the copy button.
		seg := js[pathlistIdx:]
		if cut := strings.Index(seg, "md-code-actions"); cut > 0 {
			seg = seg[:cut]
		}
		if strings.Contains(seg, "<pre") {
			t.Error("path-list rows must not be inside <pre> — scanEventForFileRefs skips <pre> descendants, which would suppress the preview/download buttons this fix exists to add")
		}
	}

	// 5. copy must join all rows. _codeBlockInfo has to special-case
	// .md-pathlist (one <code> per row) or copy would grab only the first.
	infoIdx := strings.Index(js, "function _codeBlockInfo(btn)")
	if infoIdx < 0 {
		t.Fatal("_codeBlockInfo not found")
	}
	infoRest := js[infoIdx:]
	infoEnd := strings.Index(infoRest[1:], "\nfunction ")
	if infoEnd < 0 {
		infoEnd = len(infoRest)
	}
	infoBody := infoRest[:infoEnd]
	if !strings.Contains(infoBody, "md-pathlist") {
		t.Error("_codeBlockInfo must special-case .md-pathlist so the copy button joins every path row, not just the first <code>")
	}
}

// TestDashboardHTML_PathlistStyles pins the CSS for the path-list fence so the
// rows render as a readable vertical list rather than collapsing inline.
func TestDashboardHTML_PathlistStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	if !strings.Contains(html, ".md-pathlist{") {
		t.Error("dashboard.html must define .md-pathlist styles for the path-list fence")
	}
	if !strings.Contains(html, ".md-pathline") {
		t.Error("dashboard.html must define .md-pathline styles for individual path rows")
	}
}
