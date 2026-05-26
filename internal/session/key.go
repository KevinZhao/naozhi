package session

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/keyspec"
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
	// R239-ARCH-G (#900): canonical declaration lives in
	// internal/keyspec.ProjectKeyPrefix; the session-local constant is a
	// re-export so existing callers (the keyNamespaces table below,
	// isPlannerKey, plannerNameFromKey) keep using the
	// session-package symbol without an import cycle.
	ProjectKeyPrefix = keyspec.ProjectKeyPrefix
	// ScratchKeyPrefix is already defined in scratch.go; listed here only in
	// documentation for grep-ability. Do not redefine.
	// SysKeyPrefix is used for naozhi-internal background daemon sessions.
	// Key shape is "sys:{daemon-name}" where {daemon-name} matches
	// `^[a-z][a-z0-9-]{1,30}$`. See docs/rfc/system-session.md and
	// internal/sysession/registry.go (BuiltinDaemons name validation).
	//
	// Phase 1 daemons typically do NOT register a stub at all — Runner-style
	// daemons (AutoTitler) only spawn transient claude -p subprocesses and
	// never need a long-lived ManagedSession. The prefix is reserved here
	// for future daemons that DO need a persistent stub (e.g. a metrics
	// aggregator that must survive across ticks).
	SysKeyPrefix = "sys:"
)

// keyNamespace captures the per-prefix policy bits in one row so the reserved
// + exempt classifications stay co-defined. R239-ARCH-L: previously the
// "is this a reserved namespace" list lived in key.go and the "is this a
// TTL-exempt namespace" list lived in router_core.go, with comments asking
// each side to update the other. They drifted in practice — adding a new
// reserved prefix without updating exemptKeyPrefixes silently routed the
// new namespace through the regular TTL eviction path even though the
// namespace was "non-IM-shape" by definition (cron / project / sys are
// exempt; only scratch is reserved-but-not-exempt because it's
// deliberately short-lived).
//
// Single source of truth: keyNamespaces below. Both reservedKeyPrefixes
// (key.go) and exemptKeyPrefixes (router_core.go) derive from it so a
// new namespace is added in exactly one row.
type keyNamespace struct {
	prefix string
	// exempt means alive sessions under this prefix bypass TTL eviction,
	// LRU pressure, and the active-process counter. ScratchKeyPrefix is
	// the canonical reserved-but-not-exempt entry: scratch sessions are
	// ephemeral and MUST stay subject to TTL so abandoned scratch
	// conversations release their process slot.
	exempt bool
	// kind is the per-namespace bucket label used by the exempt sub-quota
	// gate (R242-ARCH-2 / exemptCapFor). For non-exempt namespaces this is
	// the empty string and exemptKind never observes the row.
	kind string
}

// keyNamespaces is the authoritative table of reserved-namespace prefixes.
// Both `reservedKeyPrefixes` (consumers: IsReservedNamespace, IsUserVisibleKey)
// and `exemptKeyPrefixes` + `exemptKind` (consumers: isExemptKey,
// router_core.go::exemptKind, exemptCapFor) derive from this table.
// Kept sorted for grep stability.
//
// When adding a new entry, update:
//   - DESIGN.md §"Session key namespace"
//   - the exempt flag below (true = bypass TTL/LRU; default false)
//   - kind below if exempt=true (label flows into exemptCapFor switch)
//   - the sidebar / persistence filter if the new namespace should not be
//     persisted / displayed in the default UI
//   - if exempt=true, add a sub-quota cap in router_core.go's exemptCapFor
//     (otherwise spawnSession routes through maxExemptSessions fallback).
var keyNamespaces = []keyNamespace{
	{prefix: CronKeyPrefix, exempt: true, kind: "cron"},
	{prefix: ProjectKeyPrefix, exempt: true, kind: "project"},
	{prefix: ScratchKeyPrefix, exempt: false, kind: ""},
	{prefix: SysKeyPrefix, exempt: true, kind: "sys"},
}

// reservedKeyPrefixes is the list of namespaces that do NOT follow the
// standard IM key shape, derived from keyNamespaces. Use the slice rather
// than iterating keyNamespaces directly for hot-path callers — Go does not
// elide the struct-field load even on each iteration of a small slice, and
// grep-locality matches the prior single-file declaration.
var reservedKeyPrefixes = func() []string {
	out := make([]string, len(keyNamespaces))
	for i, ns := range keyNamespaces {
		out[i] = ns.prefix
	}
	return out
}()

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

// IsUserVisibleKey reports whether the key represents a session a human
// user should see in the generic session list / history panel / project
// sidebar.  The negation of IsReservedNamespace:  cron / project /
// scratch / sys keys all live in dedicated UI surfaces (cron panel,
// project sidebar groupings, scratch drawer, system drawer) and must
// be hidden from the catch-all "recent sessions" view.
//
// Keep this function as the single source of truth for "should this
// key appear in a generic listing":  every new listing API must consult
// it instead of re-growing strings.HasPrefix checks per call site.
// R245-ARCH (cron+sys hide-from-history).
func IsUserVisibleKey(key string) bool {
	return !IsReservedNamespace(key)
}

// IsCronKey reports whether the key belongs to the cron namespace. See
// CronKeyPrefix.
func IsCronKey(key string) bool {
	return strings.HasPrefix(key, CronKeyPrefix)
}

// CronKey synthesises the session key for a cron job. Keep cron-namespace
// key construction in one place so prefix changes (e.g. a future v2
// namespace) only need to touch CronKeyPrefix here — callers in the cron
// package previously inlined `"cron:" + id`, which the linker could not
// detect if the constant drifted.
func CronKey(id string) string {
	return CronKeyPrefix + id
}

// IsSysKey reports whether the key belongs to the system-daemon namespace.
// See SysKeyPrefix.
func IsSysKey(key string) bool {
	return strings.HasPrefix(key, SysKeyPrefix)
}

// plannerKeyFor is the session-package local accessor for the planner
// key shape. Kept unexported because external callers should continue
// to use internal/project's public API. KeyResolver needs to construct
// planner keys without importing project (reverse dependency), so the
// session package delegates to internal/keyspec for the canonical
// constructor — that package is zero-dep so any consumer can take it
// without an import cycle.
//
// R239-ARCH-G (#900): pre-extraction this function held the literal
// "project:{name}:planner" in two places (here and in internal/project)
// kept in sync only via cross-module hardcoded tests. The literal now
// lives once in internal/keyspec.PlannerKeyFor; both call sites
// delegate.
func plannerKeyFor(name string) string {
	return keyspec.PlannerKeyFor(name)
}

// isPlannerKey is the session-package local accessor for planner-key
// detection. Delegates to internal/keyspec.IsPlannerKey so the
// "project:" + ":planner" + non-empty-name rule is encoded exactly
// once. R239-ARCH-G (#900).
func isPlannerKey(key string) bool {
	return keyspec.IsPlannerKey(key)
}

// plannerNameFromKey extracts {name} from "project:{name}:planner". Callers
// must have verified isPlannerKey(key) first; otherwise behaviour is undefined.
// R239-ARCH-G (#900): delegates to internal/keyspec.PlannerNameFromKey so
// the slice math lives once.
func plannerNameFromKey(key string) string {
	return keyspec.PlannerNameFromKey(key)
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
		return fmt.Errorf("session key exceeds %d-byte limit", MaxSessionKeyBytes)
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
