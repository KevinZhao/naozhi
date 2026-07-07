package server

import (
	"strings"
	"testing"
)

// TestNzUtilJS_EscJsControlChars pins [R202606j-SEC-9] (#2344): escJs in
// nz_util.js must escape C0/C1 control characters and Unicode bidi/zero-width
// format characters, not just the quote/slash/angle-bracket string-breakers.
// escJs feeds inline onclick handlers (e.g. cron_view.js cronSelectWorkspace),
// and a filesystem path containing U+2028/U+2029 (raw JS line terminators) or
// bidi overrides could otherwise break out of the literal or spoof the path.
//
// No JS engine is wired into the server test suite, so this is a structural
// contract: it pins the charCodeAt-range guard so a refactor that drops the
// control-char pass regresses loudly.
func TestNzUtilJS_EscJsControlChars(t *testing.T) {
	t.Parallel()
	data, err := nzUtilJS.ReadFile("static/nz_util.js")
	if err != nil {
		t.Fatalf("read nz_util.js: %v", err)
	}
	js := string(data)

	idx := strings.Index(js, "function escJs(s)")
	if idx < 0 {
		t.Fatal("nz_util.js missing escJs — structural anchor for control-char escape contract")
	}
	end := strings.Index(js[idx:], "\n  }")
	if end < 0 {
		t.Fatal("could not bound escJs body")
	}
	body := js[idx : idx+end]

	// The legacy string-breaker escapes must remain.
	for _, lit := range []string{`replace(/'/g`, `replace(/"/g`, `\\u003c`, `\\u003e`} {
		if !strings.Contains(body, lit) {
			t.Errorf("escJs must keep legacy escape %q ([R202606j-SEC-9], #2344)", lit)
		}
	}

	// New control/format-char pass: each guarded range must be present so a
	// dropped clause fails the contract.
	required := []string{
		"charCodeAt(0)", // per-char inspection
		"0x1f",          // C0 controls
		"0x7f",          // DEL / C1 start
		"0x9f",          // C1 end
		"0x200b",        // zero-width space
		"0x200f",        // RLM (bidi marks)
		"0x202a",        // LRE (bidi embeddings)
		"0x202e",        // RLO (bidi overrides)
		"0x2028",        // line separator (JS line terminator)
		"0x2029",        // paragraph separator
		"0x2066",        // LRI (bidi isolates)
		"0x2069",        // PDI
		"0xfeff",        // BOM
		`toString(16)`,  // emits \uXXXX
		`padStart(4`,    // zero-padded to 4 hex digits
	}
	for _, lit := range required {
		if !strings.Contains(body, lit) {
			t.Errorf("escJs control-char pass must reference %q ([R202606j-SEC-9], #2344)", lit)
		}
	}
}
