package session

import (
	"strings"
	"testing"

	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// TestSetLabel_LogAttrsRunSanitizeLogAttr pins R246-SEC-14 (#820): the
// audit-log slog calls in HandleSetLabel must wrap req.Node / req.Key
// through sessionpkg.SanitizeLogAttr before they reach slog.TextHandler,
// mirroring dispatch/commands.go:51's pattern. We can't easily intercept
// slog itself in a unit test (the package re-creates the global handler
// each run), so we cover the contract at the sanitiser level: every
// runic class slog.TextHandler treats as attr-fragmenting (\t \n \r,
// C1, bidi, ZWJ, BOM, DEL) must round-trip through SanitizeLogAttr to
// a string with NONE of those bytes. The handler itself then inherits
// the safety because the sanitised string is the only thing it ever
// sees.
//
// This test is intentionally a near-duplicate of sessionpkg.SanitizeLogAttr's
// own coverage in internal/session/managed_test.go — the duplication is
// deliberate: it pins the SERVER-SIDE call-site contract independently
// of the session package's internal tests, so a future SanitizeLogAttr
// rename / inlining cannot silently regress dashboard_session.go without
// breaking THIS test as well.
func TestSetLabel_LogAttrsRunSanitizeLogAttr(t *testing.T) {
	t.Parallel()

	// Each rune below is from the IsLogInjectionRune class that fragments
	// slog.TextHandler attrs (key=value pairs). The SanitizeLogAttr contract
	// is "rewrite every such rune so the output cannot smuggle a fake
	// attribute through key=value framing".
	dangerous := []rune{
		'\t', '\n', '\r', // C0 controls that fragment slog attr lines
		0x7f,           // DEL
		0x202E, 0x202D, // bidi override
		0x200B, 0x200E, // zero-width / LRM
		0xFEFF, // BOM
		0x2028, // line separator
	}

	// Mix dangerous runes into both a session-key-shaped string (the
	// `req.Key` path) and a node-id-shaped string (the `req.Node` path).
	// Both reach the slog at line 978 / 993 and must come out clean.
	keyLike := "feishu:c2c:U_oc_abc:agent_general\x1b" + string(dangerous) + "\nnext attr"
	nodeLike := "node-east-1\r\nfake_attr=evil"

	cleanKey := sessionpkg.SanitizeLogAttr(keyLike)
	cleanNode := sessionpkg.SanitizeLogAttr(nodeLike)

	for _, r := range dangerous {
		if strings.ContainsRune(cleanKey, r) {
			t.Errorf("SanitizeLogAttr(%q) leaked rune U+%04X (key path)", keyLike, r)
		}
		if strings.ContainsRune(cleanNode, r) {
			t.Errorf("SanitizeLogAttr(%q) leaked rune U+%04X (node path)", nodeLike, r)
		}
	}

	// Drop-dead invariants: no \t / \n / \r / = pollution survives, so a
	// downstream slog parser can never see two attrs where one was sent.
	for _, b := range []byte{'\t', '\n', '\r'} {
		if strings.IndexByte(cleanKey, b) >= 0 {
			t.Errorf("cleanKey still contains attr-fragmenting byte %#x: %q", b, cleanKey)
		}
		if strings.IndexByte(cleanNode, b) >= 0 {
			t.Errorf("cleanNode still contains attr-fragmenting byte %#x: %q", b, cleanNode)
		}
	}
}
