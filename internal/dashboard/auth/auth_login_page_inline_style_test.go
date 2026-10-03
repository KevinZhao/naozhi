package auth

import (
	"encoding/json"
	"os/exec"
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

// TestLoginPage_RefusalShowsServerReason runs the inline script under node
// against a stubbed fetch: a 400 is a refusal before the token compare
// (trusted_proxy without X-Forwarded-For), so #err must show the server's
// "error" text, as text, instead of "invalid token".
func TestLoginPage_RefusalShowsServerReason(t *testing.T) {
	scripts := extractInlineBlocks(loginPageHTML, inlineScriptRe)
	if len(scripts) != 1 {
		t.Fatalf("want 1 inline <script>, got %d", len(scripts))
	}
	if strings.Contains(scripts[0], "innerHTML") {
		t.Error("login page script writes innerHTML — the server reason must go in as text")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	harness := `
const els = {};
const el = id => els[id] || (els[id] = { value: '', textContent: '', addEventListener(_, fn) { this.onsubmit = fn; } });
globalThis.document = { getElementById: el };
globalThis.window = { location: { href: '' } };
let next;
globalThis.fetch = async () => next;
` + scripts[0] + `
async function submit(res) {
  next = res; el('token').value = 'tok';
  await el('login-form').onsubmit({ preventDefault() {} });
  return el('err').textContent;
}
const long = 'r'.repeat(400);
const json = (status, body) => ({ ok: false, status, json: async () => body });
(async () => {
  const got = [
    await submit(json(400, { error: '<b>x</b> trusted_proxy' })),
    await submit({ ok: false, status: 400, json: async () => { throw new SyntaxError('not json'); } }),
    await submit(json(400, { error: long })),
    await submit(json(401, { error: 'unauthorized' })),
    await submit(json(429, {})),
  ];
  process.stdout.write(JSON.stringify(got));
})().catch(e => { console.error(e); process.exit(1); });
`
	out, err := exec.Command(nodeBin, "-e", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output %q: %v", out, err)
	}
	want := []string{"<b>x</b> trusted_proxy", "invalid token", strings.Repeat("r", 300), "invalid token", "尝试过多，请稍后再试"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("case %d: #err = %q, want %q", i, got, want[i])
		}
	}
}
