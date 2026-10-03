package auth

import (
	"regexp"
	"strings"
	"testing"
)

// TestLoginPage_NoInlineStyleAttribute pins the fix for the visible
// "dashboard token" <label>: loginPageCSP allowlists the inline <style>
// BLOCK by hash but has no `style-src-attr` / 'unsafe-inline', so a
// style="…" ATTRIBUTE is refused by the browser (console CSP violation) and
// the visually-hidden label rendered on top of the token input in
// production. All styling must live in the hashed <style> block.
func TestLoginPage_NoInlineStyleAttribute(t *testing.T) {
	if strings.Contains(loginPageHTML, `style="`) {
		t.Error("loginPageHTML uses a style=\"…\" attribute — blocked by the hash-only style-src CSP; move the rule into the <style> block")
	}
	// The label must still be visually hidden, now via the stylesheet.
	styles := extractInlineBlocks(loginPageHTML, inlineStyleRe)
	if len(styles) == 0 {
		t.Fatal("no <style> block in loginPageHTML")
	}
	if !regexp.MustCompile(`label\[for="?token"?\]\{[^}]*position:absolute;left:-9999px`).MatchString(styles[0]) {
		t.Error("<style> block lacks the visually-hidden rule for label[for=token]")
	}
	// The CSP must never fall back to allowing inline attributes.
	if strings.Contains(loginPageCSP, "unsafe-inline") || strings.Contains(loginPageCSP, "style-src-attr") {
		t.Errorf("loginPageCSP broadened to %q — fix the markup, not the policy", loginPageCSP)
	}
}

// TestLoginPage_RateLimitedMessage: HandleLogin answers 429 when the per-IP
// limiter trips; the inline script showed "invalid token" for that too,
// sending the operator to re-check a token that was never evaluated.
func TestLoginPage_RateLimitedMessage(t *testing.T) {
	if !strings.Contains(loginPageHTML, "res.status===429") {
		t.Error("login page script does not branch on res.status===429 — a rate-limited attempt is reported as 'invalid token'")
	}
	if !strings.Contains(loginPageHTML, "尝试过多，请稍后再试") {
		t.Error("login page script lacks the 429 message 尝试过多，请稍后再试")
	}
}

// TestLoginPage_RefusalShowsServerReason: a 400 is a refusal before the token
// compare (trusted_proxy without X-Forwarded-For); the page must show the
// server's "error" text instead of "invalid token", via textContent only.
func TestLoginPage_RefusalShowsServerReason(t *testing.T) {
	scripts := extractInlineBlocks(loginPageHTML, inlineScriptRe)
	if len(scripts) != 1 {
		t.Fatalf("want 1 inline <script>, got %d", len(scripts))
	}
	js := scripts[0]
	branch := regexp.MustCompile(`if\(res\.status===400\)\{[^\n]*\(await res\.json\(\)\)\.error[^\n]*getElementById\('err'\)\.textContent=`)
	if !branch.MatchString(js) {
		t.Error("login page script does not show the 400 body's error via textContent — a trusted_proxy refusal reads as 'invalid token'")
	}
	if strings.Contains(js, "innerHTML") {
		t.Error("login page script writes innerHTML — the server reason must go in as text")
	}
}
