package server

// Security-anchor source contracts over the dashboard's static assets. This
// file is what remains of the old 7,200-line static_ux_contract_test.go after
// #2533 A2b: UX/wording/layout change-detector tests moved to the Playwright
// mock-server suite (test/e2e) or were deleted outright — DO NOT add
// regex-based source-grep tests here unless they pin a security invariant
// that a browser test cannot (escaping helpers, scheme allowlists, service
// worker ban).

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestServiceWorker_R20260602190132_SEC2_NoServiceWorkerAllowed pins the
// R20260602190132-SEC-2 (#1603) fix: handleSW must NOT emit a
// `Service-Worker-Allowed` header. The header only broadens the max SW
// scope above the script's own directory; /sw.js already lives at root so
// its default scope is "/" regardless, making the header a redundant
// explicit root-scope grant that an unauthenticated scanner could read as
// a registration hint. Removing it does not change the effective scope.
func TestServiceWorker_R20260602190132_SEC2_NoServiceWorkerAllowed(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	rec := httptest.NewRecorder()
	handleSW(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("handleSW status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Service-Worker-Allowed"); got != "" {
		t.Errorf("handleSW set Service-Worker-Allowed=%q; want it absent (R20260602190132-SEC-2 #1603 — header is a redundant root-scope grant)", got)
	}
}

// TestDashboardJS_RNEW_SEC008_DataRawAlwaysEscAttr pins the RNEW-SEC-008
// contract: every `data-raw="..."` attribute emission in dashboard.js must
// route user content through `escAttr(` — never through `renderMd(` or any
// other helper. The invariant matters because attribute escaping and HTML
// escaping differ: escAttr() encodes quote/&/< for attribute context, while
// renderMd() emits raw HTML (intended for innerHTML). Routing markdown-
// rendered HTML into an attribute would let a crafted message close the
// attribute and inject a new handler — a stored-XSS pathway. Today only the
// copy-button and ask-button in renderEvent use data-raw; this test is a
// regression gate so any future data-raw site inherits the same escaping.
func TestDashboardJS_RNEW_SEC008_DataRawAlwaysEscAttr(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	// Operate on raw source: stripJSComments is naive about `//` inside JS
	// string literals and would eat real code. `data-raw=` is a specific
	// enough token that false positives from code comments are unlikely,
	// and even if one appeared the shape-based classification below would
	// skip pure literals rather than misfire.
	js := string(data)

	// Locate every `data-raw=` occurrence. We classify the value expression
	// that follows each match.
	locs := regexp.MustCompile(`data-raw=`).FindAllStringIndex(js, -1)
	if len(locs) == 0 {
		t.Fatal("RNEW-SEC-008: no data-raw= occurrences found — test anchor is stale, verify dashboard.js still uses this attribute or remove the contract")
	}
	// Require at least the 2 currently-known sites (copy + ask). If the
	// count drops below, the audit anchor moved and the test should be
	// re-evaluated rather than silently passing zero assertions.
	if len(locs) < 2 {
		t.Errorf("RNEW-SEC-008: expected >=2 data-raw= sites (copy + ask), found %d — verify the audit is still complete", len(locs))
	}

	// Pure-literal attribute values contain no interpolation markers
	// (`$` for template literals, `` ` `` for nested template-literal
	// fragments). Those are always safe.
	literalRe := regexp.MustCompile("^data-raw=\"[^\"$`]*\"")
	// Template-literal style: `data-raw="${EXPR}"`. EXPR's first call
	// must be escAttr(.
	tmplRe := regexp.MustCompile(`^data-raw="\$\{([^}]*)\}`)
	// String-concat style inside a single-quoted or backtick-delimited
	// outer literal: `data-raw="' + ESCAPER(...)`. ESCAPER must be escAttr.
	concatRe := regexp.MustCompile("^data-raw=\"['`]\\s*\\+\\s*([A-Za-z_][A-Za-z0-9_]*)\\s*\\(")

	enforced := 0
	for _, loc := range locs {
		// 200 chars is >2x the longest current occurrence and keeps the
		// regex anchors cheap.
		end := loc[0] + 200
		if end > len(js) {
			end = len(js)
		}
		window := js[loc[0]:end]

		// Hard forbid: renderMd anywhere before the attribute's closing
		// quote. renderMd emits HTML — using it in attribute context
		// would be stored-XSS.
		if cq := findDataRawAttrValueEnd(window); cq > 0 {
			if strings.Contains(window[:cq], "renderMd(") {
				t.Errorf("RNEW-SEC-008: data-raw= value must not call renderMd() — it emits HTML, not attribute-safe text. Window: %q", window[:cq])
				continue
			}
		}

		// Case 1: pure literal — no interpolation, safe.
		if literalRe.MatchString(window) {
			enforced++
			continue
		}
		// Case 2: template literal — first ${...} must call escAttr(.
		if m := tmplRe.FindStringSubmatch(window); m != nil {
			expr := strings.TrimSpace(m[1])
			if !strings.HasPrefix(expr, "escAttr(") {
				t.Errorf("RNEW-SEC-008: data-raw=\"${...}\" must open with escAttr(, got %q", expr)
			}
			enforced++
			continue
		}
		// Case 3: string concat — first call after `' +` must be escAttr.
		if m := concatRe.FindStringSubmatch(window); m != nil {
			if m[1] != "escAttr" {
				t.Errorf("RNEW-SEC-008: data-raw=\"' + X(...) — first call must be escAttr, got %q", m[1])
			}
			enforced++
			continue
		}
		// Unknown shape: reject so a new emission pattern has to extend
		// this contract explicitly.
		t.Errorf("RNEW-SEC-008: unrecognized data-raw= value shape — extend the contract to cover it. Window: %q", window)
	}
	if enforced == 0 {
		t.Error("RNEW-SEC-008: found data-raw= sites but none were classifiable — the contract matched zero patterns, which is a test bug")
	}
}

// TestDashboardJS_EscIsPureString pins R244-SEC-P3-6: esc() must NOT use
// a shared `_escEl = document.createElement('div')` scratch element with
// textContent/innerHTML round-trip, because a recursive esc() call (custom
// toString() on `s` triggering a render hook, future getter that calls
// esc() on a sub-field, etc.) would clobber the outer call's pending
// textContent before innerHTML is read, leaking the inner value into the
// outer's HTML output. The fix replaces the shared-DOM round-trip with a
// pure-string regex chain that has no shared mutable state.
//
// We pin both halves explicitly so a future "let me revert to the
// concise textContent form" patch fails the contract test instead of
// silently re-introducing the reentrancy hazard.
func TestDashboardJS_EscIsPureString(t *testing.T) {
	t.Parallel()
	// PR-0a (RFC dashboard-cron-view-extraction): esc/escAttr/escJs moved to the
	// shared nz_util.js layer. The pure-string security contract follows the
	// implementation there.
	data, err := nzUtilJS.ReadFile("static/nz_util.js")
	if err != nil {
		t.Fatalf("read nz_util.js: %v", err)
	}
	js := string(data)

	// New shape: pure-string regex chain. The three core entities (&, <, >)
	// match the textContent → innerHTML round-trip the previous esc()
	// produced — quote handling stays in escAttr so the 171 esc() call
	// site behaviours are unchanged.
	wants := []string{
		"const _escAmpRe = /&/g;",
		"const _escLtRe = /</g;",
		"const _escGtRe = />/g;",
		".replace(_escAmpRe, '&amp;')",
		".replace(_escLtRe, '&lt;')",
		".replace(_escGtRe, '&gt;')",
	}
	for _, want := range wants {
		if !strings.Contains(js, want) {
			t.Errorf("esc() must use pure-string regex chain; missing %q", want)
		}
	}

	// Old shape: shared `_escEl = document.createElement('div')` MUST be
	// gone as live code. A future copy-paste re-introducing the variable
	// would fail here even if the function body changed shape. Mention in
	// a comment is fine (the new esc() comment references the old shape
	// for context); only the executable declaration line is forbidden.
	if strings.Contains(js, "const _escEl = document.createElement('div');") {
		t.Errorf("esc() must not declare shared scratch DOM element _escEl")
	}
	// `_escEl.textContent =` and `_escEl.innerHTML` would only appear as
	// live code if the old esc() body were resurrected. Use `=` and `;`
	// suffixes to ensure we are matching statements rather than the
	// backquote-quoted prose in the comment block above the new esc().
	if strings.Contains(js, "_escEl.textContent =") {
		t.Errorf("esc() must not assign to shared _escEl.textContent")
	}
	if strings.Contains(js, "return _escEl.innerHTML;") {
		t.Errorf("esc() must not read back shared _escEl.innerHTML")
	}
}

// TestDashboardJS_ShowGitRemoteSchemeAllowlist pins R244-SEC-P3-4: the
// showGitRemote helper that drives the `git_remote_url` toast must NOT
// hand an arbitrary URL to window.open without a scheme allowlist. ssh
// remotes (`git@host:user/repo`, `ssh://user@host/repo`) can carry
// embedded credentials (user:pass@host) that would leak via the toast
// surface or address-bar tooltip on hover, and `javascript:` URLs would
// trivially execute attacker code in the dashboard origin if the remote
// string were ever attacker-controlled (cron config, project metadata).
//
// The fix gates window.open behind a positive startsWith allowlist
// covering exactly {http://, https://, git://}. We pin both the
// allowlist constant and the gating shape so a future copy-paste cannot
// drop the leading anchor (which would let "javascript:foo http://..."
// past) or relax the scheme set without an explicit test update.
func TestDashboardJS_ShowGitRemoteSchemeAllowlist(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Allowlist must include exactly the three permitted schemes.
	wantAllowlist := "const allowed = ['https://', 'http://', 'git://'];"
	if !strings.Contains(js, wantAllowlist) {
		t.Errorf("showGitRemote must declare scheme allowlist %q", wantAllowlist)
	}

	// The window.open call must be reachable only after the safe-flag
	// check; the surrounding shape (`if (safe)`) is what the allowlist
	// gates. If a refactor moves window.open out of the gate the safe
	// branch wouldn't sit immediately above the call any more.
	if !strings.Contains(js, "if (safe) {\n    window.open(url, '_blank', 'noopener,noreferrer');") {
		t.Errorf("showGitRemote must gate window.open behind the safe flag (allowlist match)")
	}

	// Lowercased prefix probe — case-insensitive scheme matching per
	// RFC 3986 §3.1. The lowercase normalisation MUST stay so an input
	// like `JAVASCRIPT:foo` cannot bypass the lower-case allowlist.
	if !strings.Contains(js, "const lower = String(url).toLowerCase();") {
		t.Errorf("showGitRemote must lowercase the URL before scheme prefix probe")
	}
}

// TestDashboardJS_RenderMdXSSContract pins the R172-SEC-H1 (#436) audit:
// every LLM-text path through renderMd / inlineMd / renderTable must
// run through esc() / escAttr() / safeUrl() before reaching innerHTML so
// a malicious markdown payload (e.g. `[click](javascript:alert(1))`,
// `<img src=x onerror=alert(1)>`) cannot execute script in the dashboard
// origin. The escape primitives themselves already exist; the risk is
// regression — a future contributor adding a new render branch and
// forgetting the esc() wrap, or relaxing safeUrl() to accept javascript:
// URLs again. This test pins the load-bearing wraps so a regression
// fails CI rather than silently shipping an XSS surface.
//
// We deliberately test for the *presence* of the load-bearing escape
// wraps as substrings rather than executing the JS in a headless
// browser; a string-match is enough because the file is statically
// readable embed.FS content and the wraps are short, syntactically
// distinctive, and would be hard to refactor away by accident.
func TestDashboardJS_RenderMdXSSContract(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// (1) safeUrl() must reject any URL whose scheme is not http(s) or
	// a fragment-only `#...`. The current allowlist regex is the
	// load-bearing line — pin it so a future "let me add mailto:" or
	// "let me allow `/foo`" patch fails this assertion. RNEW-SEC-007.
	if !strings.Contains(js, "if (/^(https?:|#)/i.test(trimmed)) return trimmed;") {
		t.Error("safeUrl must keep its strict (https?:|#) allowlist — relaxing it (e.g. allowing mailto:, /, ?) would let a malicious markdown link sneak past the scheme gate (R172-SEC-H1 / #436)")
	}
	// safeUrl's fall-through must return a literal '#' so a rejected
	// URL emits a no-op anchor instead of the original payload.
	safeUrlIdx := strings.Index(js, "function safeUrl(u)")
	if safeUrlIdx < 0 {
		t.Fatal("safeUrl function not found in dashboard.js")
	}
	rest := js[safeUrlIdx:]
	end := strings.Index(rest[1:], "\nfunction ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]
	if !strings.Contains(body, "return '#';") {
		t.Error("safeUrl must fall through to `return '#';` for any unsafe scheme (javascript:, data:, vbscript:, …) so renderMd anchors emit a no-op href instead of the raw payload (R172-SEC-H1 / #436)")
	}

	// (2) inlineMd's markdown-link branch `[text](url)` must run the
	// captured URL through safeUrl() AND escAttr() before splicing into
	// the href attribute. Either omission re-opens the XSS surface:
	//   - skipping safeUrl() lets `javascript:alert(1)` survive into href;
	//   - skipping escAttr() lets a `"` in the URL break out of the
	//     attribute even when the scheme is benign.
	// We pin the exact substring shape used today; a refactor that keeps
	// the safety properties but reshapes the call site can update both
	// the source and the test in lockstep.
	if !strings.Contains(js, "const safe = safeUrl(url);") {
		t.Error("inlineMd's [text](url) branch must call safeUrl(url) before emitting the anchor — without it, `[click](javascript:alert(1))` would render an executable href (R172-SEC-H1 / #436)")
	}
	if !strings.Contains(js, `'<a href="' + escAttr(safe)`) {
		t.Error("inlineMd's [text](url) branch must wrap the safeUrl()'d href with escAttr() before splicing — a `\"` in the URL would otherwise break out of the attribute even when the scheme is benign (R172-SEC-H1 / #436)")
	}

	// (3) Auto-linker (bare URL → <a>) must also escAttr() the captured
	// URL into the href attribute. This path runs AFTER esc(s) so the
	// captured URL has its `&` already entity-encoded, but `"` / `'`
	// inside the URL would still escape the attribute without escAttr.
	if !strings.Contains(js, `'<a href="' + escAttr(clean)`) {
		t.Error("auto-linker must wrap the cleaned URL with escAttr() before splicing into href — defence in depth against attribute-break (R172-SEC-H1 / #436)")
	}

	// (4) Fenced code block ``` ... ``` must run the code body through
	// esc() before splicing into <code>. A regression here lets an LLM
	// that emits a code fence containing literal `<script>...</script>`
	// inject script into the page even though the *visual* contract is
	// "show the source verbatim". This is the single highest-impact
	// escape in renderMdUncached.
	if !strings.Contains(js, "<code' + langAttr + '>' + esc(code) + '</code>") {
		t.Error("renderMdUncached fenced-code branch must wrap the code body with esc() — a ```...``` block containing `<script>` would otherwise execute (R172-SEC-H1 / #436)")
	}

	// (5) Inline code spans (`...`) must esc() the captured content at
	// stash-time so the final \x00CODE\d+\x00 → <code class="md-code">
	// substitution emits already-escaped HTML. The stash regex is the
	// canonical single-line pattern; pin the esc() call inside it.
	if !strings.Contains(js, "codeTokens.push(esc(c));") {
		t.Error("inlineMd's `code` span stash must esc() the captured body before token replacement — a `<script>foo</script>` inside backticks would otherwise execute (R172-SEC-H1 / #436)")
	}

	// (6) The bold/italic regex passes operate on already-esc()'d
	// content. The security comment in dashboard.js explicitly warns
	// future contributors not to reorder. Pin the comment so a
	// well-meaning refactor that runs bold/italic BEFORE esc() trips
	// this test before it ships.
	if !strings.Contains(js, "bold/italic regex must run AFTER esc(s)") {
		t.Error("inlineMd's SECURITY CONTRACT comment for bold/italic must remain — it documents the invariant that bold/italic .+? captures span already-escaped HTML, and reordering would re-introduce XSS (R172-SEC-H1 / #436)")
	}
}

// findDataRawAttrValueEnd returns the index of the closing `"` of a
// data-raw= attribute value that begins at offset 0 of `window`. Returns
// -1 if no closing quote is found. The scan is naive — it treats the
// first `"` after the opening one as the terminator, which is correct
// for the current emission shapes (escAttr() never produces a literal `"`,
// and the outer string literals use single quotes or backticks).
func findDataRawAttrValueEnd(window string) int {
	const prefix = `data-raw="`
	if !strings.HasPrefix(window, prefix) {
		return -1
	}
	for i := len(prefix); i < len(window); i++ {
		if window[i] == '"' {
			return i
		}
	}
	return -1
}
