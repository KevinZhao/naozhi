package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_LocalFileLink_RendersAsCode pins the fix for markdown links
// that point at a local file. claude CLI routinely emits a generated file as a
// markdown link — `[数学/专题/foo.html](数学/专题/foo.html)` — rather than a
// backtick code span. safeUrl rejects the non-http target (→ '#'), so the
// anchor branch is skipped; before the fix the link collapsed to plain text,
// invisible to scanEventForFileRefs (which only walks <code>/.md-code), so the
// file never got the [↗ preview][↓ download] buttons.
//
// The fix re-renders a path-shaped rejected target as inline <code> so the
// existing file-ref scanner attaches the same buttons it gives backtick paths.
// As with the sibling path-list-fence contract test, we pin the JS contract by
// windowing on the link-render branch rather than running a live DOM (dashboard.js
// is exercised as an embedded asset; there is no JS test runner in CI).
func TestDashboardJS_LocalFileLink_RendersAsCode(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Window on the inlineMd markdown-link replace block. Anchor on the regex
	// literal so the test fails loudly if the link grammar is refactored.
	linkIdx := strings.Index(js, `s.replace(/\[([^\]]+)\]\(([^)]+)\)/g`)
	if linkIdx < 0 {
		t.Fatal("markdown link replace block not found in dashboard.js — the local-file-link fix anchor is missing")
	}
	seg := js[linkIdx:]
	// Bound the window at the next `s = s.replace(` (the bare-URL autolink pass)
	// so assertions stay scoped to the link branch.
	if cut := strings.Index(seg[1:], "Auto-link bare URLs"); cut > 0 {
		seg = seg[:cut]
	}

	// 1. The rejected-URL branch must attempt a local-file rescue rather than
	//    unconditionally collapsing to plain text.
	if !strings.Contains(seg, "isFileRefCandidate(target)") {
		t.Error("local-file-link fix missing: the safeUrl-rejected branch must test isFileRefCandidate(target) so a path-shaped link target becomes a clickable file ref")
	}

	// 2. The rescue must emit an inline <code class=\"md-code\"> so
	//    scanEventForFileRefs (which only walks <code>/.md-code) can attach
	//    preview/download buttons. A <pre> wrapper would be skipped by the
	//    scanner, so it must NOT be used here.
	if !strings.Contains(seg, `'<code class="md-code">' + target + '</code>'`) {
		t.Error("local-file-link fix must wrap the path target in <code class=\"md-code\"> (no <pre>) so the file-ref scanner attaches preview/download buttons")
	}

	// 3. SECURITY: bold/italic passes run before the link pass and can splice
	//    naozhi's own <strong>/<em> spans into the `url` capture when the
	//    target contains `**`/`*`. A real path never contains `<`; the branch
	//    must reject any `<`-bearing target so injected markup cannot reach the
	//    <code> body. Pin the guard so a refactor cannot silently drop it.
	if !strings.Contains(seg, "target.indexOf('<') === -1") {
		t.Error("local-file-link fix must reject any target containing '<' (target.indexOf('<') === -1) before rendering <code> — otherwise bold/italic-injected <strong>/<em> spans from the url capture could reach the code body")
	}

	// 3b. SECURITY: the `url` capture is already tokenized by earlier inlineMd
	//     passes — backtick-code → \x00CODE<n>\x00 and inline-math → \x00KTX<n>\x00.
	//     \x00 is non-whitespace/non-colon so it slips through isFileRefCandidate;
	//     the restore passes that run AFTER the link pass would then rewrite the
	//     sentinel into a nested <code>/<span> inside the rescue <code> (malformed
	//     HTML + a corrupted path the scanner reads). The branch must reject any
	//     \x00-bearing target. Pin the guard.
	if !strings.Contains(seg, `target.indexOf('\x00') === -1`) {
		t.Error("local-file-link fix must reject any target containing a \\x00 tokenizer sentinel (target.indexOf('\\x00') === -1) — otherwise backtick/math placeholders in the url capture expand into nested <code>/<span> inside the rescue <code>")
	}

	// 3c. The rescue must apply the same FILE_REF_HAS_EXT extension gate that
	//     fencedPathList uses, so slash-shaped non-files (dates `2024/01/02`,
	//     fractions `1/2`, extension-less doc slugs) don't hijack the link into a
	//     bogus file ref with the author's label discarded. Without this the
	//     link branch and fencedPathList have inconsistent acceptance sets.
	if !strings.Contains(seg, "FILE_REF_HAS_EXT.test(base)") {
		t.Error("local-file-link fix must gate on FILE_REF_HAS_EXT.test(base) (the same extension check fencedPathList applies) so dates/fractions/extension-less slugs aren't hijacked into a file ref with the label discarded")
	}

	// 4. The fix must live strictly inside the `safe === '#'` rejected branch —
	//    it must not perturb the accepted-URL anchor render. Assert the anchor
	//    branch still emits an <a class=\"md-link\">.
	if !strings.Contains(seg, `<a href="' + escAttr(safe) + '" class="md-link"`) {
		t.Error("accepted-URL branch must still render an <a class=\"md-link\"> anchor — the local-file-link fix must not disturb normal http links")
	}
	guardIdx := strings.Index(seg, "if (safe === '#')")
	anchorIdx := strings.Index(seg, `<a href="' + escAttr(safe)`)
	codeIdx := strings.Index(seg, `'<code class="md-code">' + target + '</code>'`)
	if guardIdx < 0 || anchorIdx < 0 || codeIdx < 0 || !(guardIdx < codeIdx && codeIdx < anchorIdx) {
		t.Error("the <code> rescue must sit inside the safe === '#' rejected branch, before the accepted-URL <a> render")
	}
}
