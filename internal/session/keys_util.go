package session

import (
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// maxKeyComponent is the maximum length of a single session key component.
const maxKeyComponent = 128

// sanitizeKeyComponent truncates and strips colons from a session key component
// to prevent key confusion and unbounded map key growth.
//
// Fast path: most components are short ASCII without colons; avoid the
// ReplaceAll+RuneCount allocations in that common case.
func sanitizeKeyComponent(s string) string {
	if len(s) <= maxKeyComponent {
		ok := true
		for i := 0; i < len(s); i++ {
			c := s[i]
			// Reject colons (key separator), 8-bit bytes, all C0 controls and
			// DEL: IM-originated IDs reach slog.TextHandler attrs, where \n
			// forges entries, \x1b rewrites terminals and \t splits an attr
			// in two. Must stay byte-for-byte equivalent to the slow path below.
			if c == ':' || c >= 0x80 || c < 0x20 || c == 0x7f {
				ok = false
				break
			}
		}
		if ok {
			return s
		}
	}
	s = strings.ReplaceAll(s, ":", "_")
	// Slow path: map C0/DEL/C1 controls and Unicode bidi / zero-width / BOM
	// codepoints to '_' via the shared deny-set (sessionkey/denyset.go). C1
	// codepoints arrive as valid UTF-8 (0xC2 0x80..0x9F) and so bypass the
	// fast-path byte gate; do not re-inline the deny-set here (#2301).
	s = strings.Map(sessionkey.SanitizeKeyRune, s)
	// UTF-8 byte length ≥ rune count, so only pay for RuneCountInString and
	// the []rune conversion when the byte length actually exceeds the cap.
	if len(s) > maxKeyComponent && utf8.RuneCountInString(s) > maxKeyComponent {
		runes := []rune(s)
		s = string(runes[:maxKeyComponent])
	}
	return s
}

// SanitizeLogAttr returns a version of s that is safe to feed directly into
// slog attributes without fragmenting log lines (same rules as session-key
// components). Call it on any IM-originated string (chat ID, user ID, raw
// incoming key) BEFORE passing it to slog so an attacker-controlled ID
// cannot inject \n, tabs, or ANSI into operator log streams.
func SanitizeLogAttr(s string) string {
	return sanitizeKeyComponent(s)
}

// SanitizeCWDKey converts a filesystem path to a safe session-key component
// by stripping the leading slash, replacing path separators and colons,
// and truncating to maxKeyComponent.
func SanitizeCWDKey(cwd string) string {
	s := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	return sanitizeKeyComponent(s)
}

// SessionKey builds a session key from components.
func SessionKey(platform, chatType, id, agentID string) string {
	if agentID == "" {
		agentID = "general"
	}
	return sanitizeKeyComponent(platform) + ":" + sanitizeKeyComponent(chatType) + ":" + sanitizeKeyComponent(id) + ":" + sanitizeKeyComponent(agentID)
}

// TakeoverKey builds a session key for a takeover from a discovered
// process CWD.
//
// cwdKey MUST already be sanitized (e.g. via SanitizeCWDKey): it is
// concatenated directly into the colon-delimited key without re-running
// sanitizeKeyComponent, so a raw path containing ':' would produce a
// malformed key.
func TakeoverKey(cwdKey string) string {
	return "local:takeover:" + cwdKey + ":general"
}
