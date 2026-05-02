package session

import (
	"strings"
	"testing"
)

// TestSanitizeKeyComponent_StripsTab asserts that tab (U+0009) is stripped in
// both the fast path and the slow path. slog.TextHandler uses tab as the
// key/value separator, so a tab in an IM-originated chat ID would fragment
// one attr into two. R60-GO-M1.
func TestSanitizeKeyComponent_StripsTab(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in string
	}{
		{"fast-path plain ASCII with tab", "ab\tcd"},
		{"fast-path colon triggers slow path, tab still stripped", "ab\tcd:xx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeKeyComponent(c.in)
			if strings.ContainsRune(got, '\t') {
				t.Errorf("sanitize(%q) = %q, still contains tab", c.in, got)
			}
		})
	}
}

// TestSanitizeLogAttr_NoLogFragmentation covers the full set of byte classes
// that slog.TextHandler treats specially: newlines, tabs, ANSI escape, and
// Unicode bidi/zero-width. R60-GO-H1.
func TestSanitizeLogAttr_NoLogFragmentation(t *testing.T) {
	t.Parallel()
	bad := "user\nadmin=1\tpassword\x1b[31mevil‮reverse​hidden"
	got := SanitizeLogAttr(bad)
	for _, r := range []rune{'\n', '\t', 0x1b, 0x202E, 0x200B} {
		if strings.ContainsRune(got, r) {
			t.Errorf("SanitizeLogAttr(%q) = %q, still contains U+%04X", bad, got, r)
		}
	}
}

// TestSanitizeLogAttr_StripsC1Controls covers R61-GO-6: the fast-path byte
// gate rejects 8-bit bytes, but a chat ID arriving as valid UTF-8 encoding
// of U+0080..U+009F (two-byte sequence 0xC2 0x80 .. 0xC2 0x9F) takes the
// slow path, where the rune is < 0xA0. Terminals interpret C1 codepoints
// as control functions, so SanitizeLogAttr must strip them explicitly.
func TestSanitizeLogAttr_StripsC1Controls(t *testing.T) {
	t.Parallel()
	// U+0085 NEL (Next Line) and U+0088 HTS are terminal control functions.
	in := "useridok"
	got := SanitizeLogAttr(in)
	for _, r := range []rune{0x85, 0x88} {
		if strings.ContainsRune(got, r) {
			t.Errorf("SanitizeLogAttr(%q) = %q, still contains C1 U+%04X", in, got, r)
		}
	}
	// U+007F DEL is equally dangerous in some terminals.
	if strings.ContainsRune(SanitizeLogAttr("a\x7fb"), 0x7f) {
		t.Error("SanitizeLogAttr must strip DEL (U+007F)")
	}
}

func TestSessionKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		platform, chatType, id, agentID string
		expected                        string
	}{
		{"feishu", "direct", "alice", "general", "feishu:direct:alice:general"},
		{"feishu", "group", "xxx", "code-reviewer", "feishu:group:xxx:code-reviewer"},
		{"feishu", "direct", "bob", "", "feishu:direct:bob:general"},
		{"telegram", "direct", "user1", "researcher", "telegram:direct:user1:researcher"},
	}

	for _, tt := range tests {
		got := SessionKey(tt.platform, tt.chatType, tt.id, tt.agentID)
		if got != tt.expected {
			t.Errorf("SessionKey(%q,%q,%q,%q) = %q, want %q",
				tt.platform, tt.chatType, tt.id, tt.agentID, got, tt.expected)
		}
	}
}
