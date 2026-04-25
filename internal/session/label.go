package session

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxUserLabelBytes caps the operator-set session label. 128 bytes covers any
// realistic sidebar/header title while keeping sessions.json growth bounded —
// the label is rebroadcast on every /api/sessions poll, so a megabyte-scale
// string would multiply dashboard egress N×(tabs).
const MaxUserLabelBytes = 128

// ValidateUserLabel trims surrounding whitespace, enforces MaxUserLabelBytes,
// rejects invalid UTF-8, and blocks ASCII / C1 control characters that would
// otherwise corrupt slog JSONHandler output, terminal log viewers, or
// dashboard HTML. An empty return value is the caller's signal to clear any
// prior label.
//
// Shared by internal/server (dashboard HTTP path) and internal/upstream
// (reverse-RPC worker) so both trust boundaries apply identical rules. The
// upstream path is load-bearing against a compromised control-node.
func ValidateUserLabel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return "", nil
	}
	if len(s) > MaxUserLabelBytes {
		return "", errors.New("label too long")
	}
	if !utf8.ValidString(s) {
		return "", errors.New("invalid utf-8")
	}
	for _, r := range s {
		// Reject C0 (U+0000..U+001F), DEL (U+007F), and C1 (U+0080..U+009F)
		// control ranges. Tab is NOT exempted: slog.TextHandler uses tab as
		// its key/value separator, so a tab inside a label that later flows
		// into a log attr would fragment the output. Mirrors
		// sanitizeKeyComponent's gate.
		if r == 0 || r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return "", errors.New("control characters not allowed")
		}
	}
	return s, nil
}
