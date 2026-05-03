package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_SanitizeKeySlug_UnicodeColons pins the Round 170 fix:
// sanitizeKeySlug must strip Unicode colon lookalikes so a project folder
// containing e.g. 'foo：bar' (FULLWIDTH COLON U+FF1A) cannot survive as a
// colon-like character into the 4-segment SessionKey that
// strings.SplitN(key, ":", 4) relies on server-side. The pre-fix regex
// only stripped ASCII ':' and whitespace / filesystem chars; U+FF1A is
// outside U+0000–U+007F and was being passed through.
//
// Also pins the bidi override strip so work_dir-style log-injection
// payloads do not sneak in via sidebar / key fallback displays.
func TestDashboardJS_SanitizeKeySlug_UnicodeColons(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Window the search to the sanitizeKeySlug body so an unrelated regex
	// elsewhere (e.g. message-rendering) doesn't produce a false positive.
	fnIdx := strings.Index(js, "function sanitizeKeySlug(s)")
	if fnIdx < 0 {
		t.Fatal("sanitizeKeySlug function not found in dashboard.js")
	}
	// Bound by the next top-level function keyword. Best-effort — function
	// declarations in dashboard.js are all at col 0.
	rest := js[fnIdx:]
	end := strings.Index(rest[1:], "\nfunction ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// Must strip at least the FULLWIDTH COLON codepoint. We pin the
	// character class rather than the specific regex syntax so Prettier
	// is free to rewrite the literal — as long as U+FF1A is somewhere in
	// the substitution class, the fix holds.
	if !strings.Contains(body, "：") {
		t.Error("sanitizeKeySlug must strip U+FF1A FULLWIDTH COLON — a project folder named 'foo：bar' would otherwise survive into the 4-segment key")
	}

	// Must strip the bidi override / embedding / isolate block. Matching
	// any of the visible characters in the class is sufficient to confirm
	// the intent — if Prettier rewrites to ‪–‮ notation a future
	// review will recognize the range.
	hasBidi := strings.ContainsAny(body, "‪‫‬‭‮") ||
		strings.Contains(body, `\u202`) ||
		strings.Contains(body, `⁦`)
	if !hasBidi {
		t.Error("sanitizeKeySlug must strip bidi override / embedding characters (U+202A–U+202E, U+2066–U+2069) — RLO etc. would otherwise corrupt sidebar and log rendering")
	}
}

// TestDashboardJS_CopyStringToClipboard_FinallyDetach pins the Round 170
// fix to copyStringToClipboard: the fallback <textarea> must be removed
// in a finally block (not an inline removeChild after execCommand) so a
// thrown execCommand (sandboxed iframe, locked clipboard) does not leak a
// textarea containing the copied string into the DOM for the page's
// lifetime.
func TestDashboardJS_CopyStringToClipboard_FinallyDetach(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	fnIdx := strings.Index(js, "async function copyStringToClipboard(s)")
	if fnIdx < 0 {
		t.Fatal("copyStringToClipboard function not found")
	}
	rest := js[fnIdx:]
	end := strings.Index(rest[1:], "\nfunction ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// The finally block must contain a removeChild / equivalent teardown.
	if !strings.Contains(body, "finally") {
		t.Error("copyStringToClipboard must use try/finally to detach the fallback textarea — a thrown execCommand would otherwise leak a textarea into the DOM")
	}
	// parentNode check in the finally body defends against the first
	// appendChild failing (a real edge case in Shadow DOM hosts).
	if !strings.Contains(body, "parentNode") {
		t.Error("copyStringToClipboard's finally branch should gate removeChild on parentNode — avoids a TypeError when the textarea was never attached")
	}
}
