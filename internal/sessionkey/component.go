package sessionkey

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxKeyComponent is the maximum length of a single session key component.
const MaxKeyComponent = 128

// SanitizeKeyComponent truncates and strips colons from a session key component
// to prevent key confusion and unbounded map key growth.
//
// Fast path: most components are short ASCII without colons; avoid the
// ReplaceAll+RuneCount allocations in that common case.
func SanitizeKeyComponent(s string) string {
	if len(s) <= MaxKeyComponent {
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
	// codepoints to '_' via the shared deny-set (denyset.go). C1
	// codepoints arrive as valid UTF-8 (0xC2 0x80..0x9F) and so bypass the
	// fast-path byte gate; do not re-inline the deny-set here (#2301).
	s = strings.Map(SanitizeKeyRune, s)
	// UTF-8 byte length ≥ rune count, so only pay for RuneCountInString and
	// the []rune conversion when the byte length actually exceeds the cap.
	if len(s) > MaxKeyComponent && utf8.RuneCountInString(s) > MaxKeyComponent {
		runes := []rune(s)
		s = string(runes[:MaxKeyComponent])
	}
	return s
}

// SanitizeCWDKey converts a filesystem path to a safe session-key component
// by stripping the leading slash, replacing path separators and colons,
// and truncating to MaxKeyComponent.
func SanitizeCWDKey(cwd string) string {
	s := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	return SanitizeKeyComponent(s)
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

// MaxSessionKeyBytes caps the byte length of a session key accepted over any
// trust boundary: 4 components of MaxKeyComponent bytes each (enforced by
// sanitizeKeyComponent on IM-path construction) plus 3 separators.
const MaxSessionKeyBytes = 4*MaxKeyComponent + 3

// ValidateSessionKey rejects session keys that contain control bytes, non-UTF-8
// sequences, or exceed MaxSessionKeyBytes. The IM path silently sanitizes
// (sanitizeKeyComponent) because operators cannot influence inbound chat IDs;
// reverse-RPC / HTTP paths must reject outright so a compromised control-node
// or dashboard caller cannot inject keys that corrupt slog output, terminal
// log viewers, or sessions.json storage.
//
// Empty keys are rejected — callers wanting to short-circuit them must do so first.
func ValidateSessionKey(k string) error {
	if k == "" {
		return errors.New("empty session key")
	}
	if len(k) > MaxSessionKeyBytes {
		return fmt.Errorf("session key exceeds %d-byte limit", MaxSessionKeyBytes)
	}
	if !utf8.ValidString(k) {
		return errors.New("session key invalid utf-8")
	}
	for _, r := range k {
		switch {
		case IsControlKeyRune(r):
			return errors.New("session key contains control character")
		case IsInvisibleKeyRune(r):
			return errors.New("session key contains invisible control character")
		}
	}
	// Deliberately does NOT enforce a 4-segment shape: cross-node protocols
	// (internal/upstream) forward operator-supplied keys of unknown shape so
	// router.GetSession can report the absence. Call sites relying on 4
	// segments (promote, ChatKey extraction) must do their own split check.
	return nil
}
