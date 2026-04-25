package session

import (
	"errors"
	"unicode/utf8"
)

// MaxSessionKeyBytes caps the byte length of a session key accepted over any
// trust boundary. A key has the shape `{platform}:{chatType}:{id}:{agentID}`,
// and each component is individually capped at maxKeyComponent (128 bytes) by
// sanitizeKeyComponent on IM-path construction. The `4 * 128 + 3 separators`
// ceiling gives a safe upper bound for validators at RPC / HTTP entrypoints.
const MaxSessionKeyBytes = 4*maxKeyComponent + 3

// ValidateSessionKey rejects session keys that contain control bytes, non-UTF-8
// sequences, or exceed MaxSessionKeyBytes. It mirrors the per-component gate
// enforced by sanitizeKeyComponent for IM-originated keys — the IM path
// silently sanitizes (because operators cannot influence inbound chat IDs),
// but the reverse-RPC / HTTP paths must reject outright so a compromised
// control-node or dashboard caller cannot inject keys that corrupt slog
// output, terminal log viewers, or sessions.json storage. R65-SEC-M-2.
//
// Empty and missing keys are rejected — callers that want to short-circuit
// empty keys must do so themselves before calling this.
func ValidateSessionKey(k string) error {
	if k == "" {
		return errors.New("empty session key")
	}
	if len(k) > MaxSessionKeyBytes {
		return errors.New("session key too long")
	}
	if !utf8.ValidString(k) {
		return errors.New("session key invalid utf-8")
	}
	for _, r := range k {
		// Reject C0 (U+0000..U+001F including tab), DEL (U+007F), and the
		// C1 control range (U+0080..U+009F). Keys travel directly into
		// slog.TextHandler attrs and sessions.json — a tab fragments a
		// log attr into two, \n injects fake log lines, and C1 codepoints
		// are interpreted as control functions by some terminal emulators.
		// Also reject the Unicode bidi / zero-width classes that
		// sanitizeKeyComponent drops on the IM path.
		if r == 0 || r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return errors.New("session key contains control character")
		}
		switch {
		case r >= 0x200B && r <= 0x200F, // zero-width / LTR-RTL marks
			r >= 0x202A && r <= 0x202E, // bidi embedding / override
			r == 0x2028, r == 0x2029,   // line / paragraph separator
			r == 0xFEFF: // BOM
			return errors.New("session key contains invisible control character")
		}
	}
	return nil
}
