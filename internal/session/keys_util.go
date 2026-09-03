package session

import (
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// R229-CR-6: extracted from managed.go so the session-key construction /
// sanitisation utilities live alongside the namespace-prefix policy table
// (reserved_keys.go) instead of being buried inside the 1500-line
// managed.go god-file. Pure move — no behaviour change.

// maxKeyComponent is the maximum length of a single session key component.
const maxKeyComponent = 128

// sanitizeKeyComponent truncates and strips colons from a session key component
// to prevent key confusion and unbounded map key growth.
//
// Fast path: most session-key components are short ASCII without colons
// (platform IDs, agent names, chat IDs). Avoid ReplaceAll+RuneCount allocations
// in that common case.
func sanitizeKeyComponent(s string) string {
	if len(s) <= maxKeyComponent {
		ok := true
		for i := 0; i < len(s); i++ {
			c := s[i]
			// Reject colons (reserved key separator), 8-bit bytes (non-ASCII
			// IDs are truncated to maxKeyComponent via the rune path below),
			// and ALL C0 control bytes including tab, plus DEL (0x7f). Control
			// bytes can travel through IM-originated chat IDs into
			// slog.TextHandler attrs and fragment log lines: \n injects fake
			// entries, \x1b rewrites terminal output via ANSI, and \t is the
			// key=value separator for slog.TextHandler — a tab in a chat ID
			// would split one attr into two. The slow path (strings.Map
			// below) mirrors this gate byte-for-byte so the two paths agree.
			// R60-GO-M1 / R61-GO-6.
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
	// Map every codepoint a session key may not contain to '_' (rationale
	// per class lives on sessionkey/denyset.go):
	//   - ALL C0 controls including tab — slog.TextHandler uses tab as the
	//     key/value separator so an embedded tab would fragment one attr
	//     into two. Matches the fast-path gate above. R60-GO-M1.
	//   - DEL + C1 (U+007F..U+009F). The fast-path byte gate rejects 8-bit
	//     *bytes*, but a chat ID that arrives as valid UTF-8 containing a C1
	//     codepoint (0xC2 0x80..0xC2 0x9F) takes the slow path because the
	//     first byte (0xC2) is ≥ 0x80; without this the C1 codepoint survives
	//     and terminals may interpret it as a control function. R61-GO-6.
	//   - Unicode bidi / zero-width / LS-PS / BOM classes that terminal log
	//     viewers render as invisible or swap-displayed. Not covered by the
	//     C0 gate in the fast path.
	// Done via strings.Map because the ReplaceAll-based fast path is 1:1
	// on bytes; rune-truncation below handles any multi-byte tail.
	// R202606f-ARCH-6 (#2301): do not re-inline the deny-set here.
	s = strings.Map(sessionkey.SanitizeKeyRune, s)
	// Cheap byte-length gate first: UTF-8 byte length is always ≥ rune count,
	// so strings with ≤ maxKeyComponent bytes cannot exceed maxKeyComponent
	// runes. Only pay for RuneCountInString + []rune conversion when byte
	// length actually exceeds the cap. The common case (sanitize reached
	// only because of a colon or embedded control byte) skips both allocs.
	// R64-PERF-8.
	if len(s) > maxKeyComponent && utf8.RuneCountInString(s) > maxKeyComponent {
		runes := []rune(s)
		s = string(runes[:maxKeyComponent])
	}
	return s
}

// SanitizeLogAttr returns a version of s that is safe to feed directly into
// slog attributes without fragmenting log lines. Uses the same rules as
// session-key components: strips colons, 8-bit bytes, C0 control bytes, and
// Unicode bidi/zero-width chars; truncates to maxKeyComponent runes. Call
// this on any IM-originated string (chat ID, user ID, raw incoming key)
// BEFORE passing it to slog.With / slog.*Context so an attacker-controlled
// chat ID cannot inject \n, tabs, or ANSI into operator log streams.
// R60-GO-H1.
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
// cwdKey MUST already be sanitized (e.g. via SanitizeCWDKey) — TakeoverKey
// concatenates it directly into the colon-delimited session key without
// re-running sanitizeKeyComponent, so a raw path containing ':' or
// other key separators would produce a malformed key.  Callers
// (server/dashboard_discovered.go, upstream/connector_rpc.go) pass
// SanitizeCWDKey output here.
func TakeoverKey(cwdKey string) string {
	return "local:takeover:" + cwdKey + ":general"
}
