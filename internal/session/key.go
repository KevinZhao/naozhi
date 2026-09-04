package session

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// MaxSessionKeyBytes caps the byte length of a session key accepted over any
// trust boundary: 4 components of maxKeyComponent bytes each (enforced by
// sanitizeKeyComponent on IM-path construction) plus 3 separators.
const MaxSessionKeyBytes = 4*maxKeyComponent + 3

// Reserved session-key namespace prefixes.
//
// Canonical IM keys are `{platform}:{chatType}:{id}:{agentID}` (DESIGN.md
// §"Session key"); these namespaces deliberately escape that schema and must
// be filtered / routed specially. Each prefix is a full token (trailing colon)
// so "cronographer:..." cannot be misclassified as cron-owned. The constants
// are owned by the leaf internal/sessionkey package and re-exported here for
// external callers (docs/rfc/cron-sysession-merge.md Phase A2 / B §3.3.5).
const (
	// CronKeyPrefix re-exports sessionkey.CronKeyPrefix.
	CronKeyPrefix = sessionkey.CronKeyPrefix
	// ProjectKeyPrefix is used for project-scoped planner sessions
	// ("project:{name}:planner", see internal/project.IsPlannerKey) (#1412).
	ProjectKeyPrefix = sessionkey.ProjectKeyPrefix
	// SysKeyPrefix re-exports sessionkey.SysKeyPrefix.
	SysKeyPrefix = sessionkey.SysKeyPrefix
)

// keyNamespace is one row of the reserved-namespace policy table. Reserved
// and TTL-exempt classification are co-defined here so a new namespace cannot
// be added to one list and forgotten in the other.
type keyNamespace struct {
	prefix string
	// exempt: alive sessions under this prefix bypass TTL eviction, LRU
	// pressure and the active-process counter. Scratch is reserved but NOT
	// exempt so abandoned scratch conversations release their process slot.
	exempt bool
	// kind is the bucket label used by the exempt sub-quota gate
	// (exemptCapFor); "" for non-exempt rows.
	kind string
}

// keyNamespaces is the authoritative reserved-namespace table; reservedKeyPrefixes,
// exemptKeyPrefixes and exemptKind derive from it. Kept sorted.
//
// When adding an entry also update: DESIGN.md §"Session key namespace"; the
// sidebar / persistence filter if it must not be shown by default; and, if
// exempt=true, a sub-quota cap in router_core.go's exemptCapFor (otherwise
// spawnSession falls back to maxExemptSessions).
var keyNamespaces = []keyNamespace{
	{prefix: CronKeyPrefix, exempt: true, kind: "cron"},
	{prefix: ProjectKeyPrefix, exempt: true, kind: "project"},
	{prefix: ScratchKeyPrefix, exempt: false, kind: ""},
	{prefix: SysKeyPrefix, exempt: true, kind: "sys"},
}

// reservedKeyPrefixes lists the namespaces that do NOT follow the standard IM
// key shape, derived from keyNamespaces (flat slice for hot-path callers).
var reservedKeyPrefixes = func() []string {
	out := make([]string, len(keyNamespaces))
	for i, ns := range keyNamespaces {
		out[i] = ns.prefix
	}
	return out
}()

// IsReservedNamespace reports whether the given key belongs to any reserved
// namespace. Prefer IsCronKey / project.IsPlannerKey / IsScratchKey when the
// specific namespace matters.
func IsReservedNamespace(key string) bool {
	for _, prefix := range reservedKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// IsCronKey reports whether the key belongs to the cron namespace.
//
// Deprecated: use sessionkey.IsCronKey.
func IsCronKey(key string) bool { return sessionkey.IsCronKey(key) }

// CronKey synthesises the session key for a cron job.
//
// Deprecated: use sessionkey.CronKey.
func CronKey(id string) string { return sessionkey.CronKey(id) }

// IsSysKey reports whether the key belongs to the system-daemon namespace.
//
// Deprecated: use sessionkey.IsSysKey.
func IsSysKey(key string) bool { return sessionkey.IsSysKey(key) }

// plannerKeyFor builds the planner key shape. Unexported: external callers use
// internal/project's API; KeyResolver needs the constructor without importing
// project (reverse dependency), so it delegates to the zero-dep
// internal/sessionkey package (#900, #1412).
func plannerKeyFor(name string) string {
	return sessionkey.PlannerKeyFor(name)
}

// isPlannerKey delegates to sessionkey.IsPlannerKey so the
// "project:" + ":planner" + non-empty-name rule is encoded once.
func isPlannerKey(key string) bool {
	return sessionkey.IsPlannerKey(key)
}

// plannerNameFromKey extracts {name} from "project:{name}:planner". Callers
// must have verified isPlannerKey(key) first; otherwise behaviour is undefined.
func plannerNameFromKey(key string) string {
	return sessionkey.PlannerNameFromKey(key)
}

// DeniedKeyRuneRanges returns the inclusive [lo, hi] codepoint ranges that
// ValidateSessionKey rejects, as a fresh slice so callers cannot mutate the
// table. Exposed for cross-layer contract tests; production code should call
// ValidateSessionKey. The table is owned by internal/sessionkey (denyset.go)
// and shared with shim.validateKeyForShim / sanitizeKeyComponent /
// SanitizeQuote (#2301); the dashboard's sanitizeKeySlug must strip the same
// set, which the internal/server contract test checks against this accessor (#2429).
func DeniedKeyRuneRanges() [][2]rune {
	return sessionkey.DeniedKeyRuneRanges()
}

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
		case sessionkey.IsControlKeyRune(r):
			return errors.New("session key contains control character")
		case sessionkey.IsInvisibleKeyRune(r):
			return errors.New("session key contains invisible control character")
		}
	}
	// Deliberately does NOT enforce a 4-segment shape: cross-node protocols
	// (internal/upstream) forward operator-supplied keys of unknown shape so
	// router.GetSession can report the absence. Call sites relying on 4
	// segments (promote, ChatKey extraction) must do their own split check.
	return nil
}
