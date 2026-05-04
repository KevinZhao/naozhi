package session

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxSessionKeyBytes caps the byte length of a session key accepted over any
// trust boundary. A key has the shape `{platform}:{chatType}:{id}:{agentID}`,
// and each component is individually capped at maxKeyComponent (128 bytes) by
// sanitizeKeyComponent on IM-path construction. The `4 * 128 + 3 separators`
// ceiling gives a safe upper bound for validators at RPC / HTTP entrypoints.
const MaxSessionKeyBytes = 4*maxKeyComponent + 3

// Reserved session-key namespace prefixes.
//
// The canonical IM session key shape is `{platform}:{chatType}:{id}:{agentID}`
// (DESIGN.md §"Session key"), but several internal subsystems synthesise keys
// that deliberately escape this schema — their platform slot is not a real IM
// platform name and they must be filtered / routed specially. Listing the
// prefixes in one place lets feature code consult a single source of truth
// instead of re-growing the same strings.HasPrefix check in every new
// subsystem. R176-ARCH-M1.
//
// Each prefix is a full token (trailing colon) so substring collisions cannot
// accidentally misclassify a key like "cronographer:..." as cron-owned.
const (
	// CronKeyPrefix is used for cron-scheduler-owned sessions. Key shape is
	// "cron:{jobID}" — see internal/cron/scheduler.go RegisterCronStub.
	CronKeyPrefix = "cron:"
	// ProjectKeyPrefix is used for project-scoped planner sessions. Key
	// shape is "project:{name}:planner" — see internal/project.IsPlannerKey.
	ProjectKeyPrefix = "project:"
	// ScratchKeyPrefix is already defined in scratch.go; listed here only in
	// documentation for grep-ability. Do not redefine.
)

// reservedKeyPrefixes is the authoritative list of namespaces that do NOT
// follow the standard IM key shape. Kept sorted for grep. When adding a new
// entry, update:
//   - DESIGN.md §"Session key namespace"
//   - exemptKeyPrefixes (router.go) if the new namespace is TTL-exempt
//   - the sidebar / persistence filter if the new namespace should not be
//     persisted / displayed in the default UI
var reservedKeyPrefixes = []string{
	CronKeyPrefix,
	ProjectKeyPrefix,
	ScratchKeyPrefix,
}

// IsReservedNamespace reports whether the given key belongs to any reserved
// namespace (cron / project / scratch). Callers should prefer the namespace-
// specific helpers (IsCronKey / project.IsPlannerKey / IsScratchKey) when
// they care which one; this umbrella check is for validators and tooling
// that only need "is this the standard IM shape or not".
func IsReservedNamespace(key string) bool {
	for _, prefix := range reservedKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// IsCronKey reports whether the key belongs to the cron namespace. See
// CronKeyPrefix.
func IsCronKey(key string) bool {
	return strings.HasPrefix(key, CronKeyPrefix)
}

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
	// Note: ValidateSessionKey does NOT enforce that the key has exactly 4
	// colon-separated segments. Cross-node protocols (internal/upstream)
	// forward operator-supplied keys whose shape may be unknown — the
	// "unknown key" path expects validation to accept arbitrary strings so
	// that downstream router.GetSession can report the absence. Call sites
	// that rely on a 4-segment shape (promote, ChatKey extraction) must do
	// their own split check.
	return nil
}
