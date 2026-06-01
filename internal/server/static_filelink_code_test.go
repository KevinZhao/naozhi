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
	//    scanner, so it must NOT be used here. The markup is produced by the
	//    shared fileRefCode helper (default class md-code) — #1512 unified the
	//    three file-ref <code> sites behind it, so pin the helper call rather
	//    than the inline string literal.
	if !strings.Contains(seg, `fileRefCode(target)`) {
		t.Error("local-file-link fix must wrap the path target via fileRefCode(target) (default class md-code, no <pre>) so the file-ref scanner attaches preview/download buttons")
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
	codeIdx := strings.Index(seg, `fileRefCode(target)`)
	if guardIdx < 0 || anchorIdx < 0 || codeIdx < 0 || !(guardIdx < codeIdx && codeIdx < anchorIdx) {
		t.Error("the fileRefCode(target) rescue must sit inside the safe === '#' rejected branch, before the accepted-URL <a> render")
	}
}

// TestDashboardJS_FileRefCode_Helper pins the shared fileRefCode helper (#1512)
// that unified the three file-ref <code> render sites: markdown-link rescue,
// CODE-token restore, and fencedPathList rows. The helper's tag/class is
// load-bearing — scanEventForFileRefs queries `code, .md-code` to attach
// preview/download buttons — so this test pins both output shapes and the
// non-escaping contract so a refactor cannot silently break button attachment.
func TestDashboardJS_FileRefCode_Helper(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. The helper must exist.
	fnIdx := strings.Index(js, "function fileRefCode(inner, className)")
	if fnIdx < 0 {
		t.Fatal("fileRefCode helper not found in dashboard.js — the #1512 unification is missing")
	}
	body := js[fnIdx:]
	if end := strings.Index(body[1:], "\nfunction "); end >= 0 {
		body = body[:end]
	}

	// 2. Default (md-code) shape: backtick spans + link rescue rely on it.
	if !strings.Contains(body, `'<code class="' + cls + '">' + inner + '</code>'`) {
		t.Error("fileRefCode must emit <code class=\"...\">inner</code> for the classed variant (the .md-code pill used by backtick/link-rescue paths)")
	}
	// 3. Bare-<code> shape: fencedPathList rows pass '' so the .md-pathline code
	//    CSS (no pill) wins; .md-code would add a background/padding pill.
	if !strings.Contains(body, "'<code>' + inner + '</code>'") {
		t.Error("fileRefCode must emit a bare <code>inner</code> when className is '' so fencedPathList rows keep their pill-free .md-pathline code styling")
	}
	// 4. Default class must be md-code when className is omitted.
	if !strings.Contains(body, "className === undefined ? 'md-code'") {
		t.Error("fileRefCode must default className to 'md-code' when the arg is omitted (the link rescue + CODE-token restore omit it)")
	}
	// 5. The helper must NOT escape inner — escaping is the callsite's job.
	//    Guard against a well-meaning refactor sneaking esc()/escAttr() in.
	if strings.Contains(body, "esc(") || strings.Contains(body, "escAttr(") {
		t.Error("fileRefCode must NOT escape inner — every callsite owns its own escaping/guarding; adding esc() here would double-escape the already-esc'd CODE-token and pathlist callers")
	}

	// 6. All three callsites must route through the helper.
	if !strings.Contains(js, "fileRefCode(target)") {
		t.Error("link-rescue site must call fileRefCode(target)")
	}
	if !strings.Contains(js, "fileRefCode(codeTokens[+idx])") {
		t.Error("CODE-token restore site must call fileRefCode(codeTokens[+idx])")
	}
	if !strings.Contains(js, `fileRefCode(esc(p.path), '')`) {
		t.Error("fencedPathList row must call fileRefCode(esc(p.path), '') — bare <code> to preserve .md-pathline code styling while still esc'ing the path")
	}
	// 7. The old divergent inline literals must be gone (so we don't regress to
	//    three hand-maintained strings).
	if strings.Contains(js, `'<code class="md-code">' + target + '</code>'`) ||
		strings.Contains(js, `'<code class="md-code">' + codeTokens[+idx] + '</code>'`) ||
		strings.Contains(js, `'<div class="md-pathline"><code>' + esc(p.path)`) {
		t.Error("the pre-#1512 inline <code> literals must be removed — every file-ref code span must go through fileRefCode")
	}
}
