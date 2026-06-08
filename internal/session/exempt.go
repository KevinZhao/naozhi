package session

import "strings"

// File: exempt.go
//
// Stateless exempt-namespace quota helpers relocated from router_core.go
// (R20260607-ARCH-4 / #1907). These are pure functions over the
// keyNamespaces table (key.go) and the maxExemptSessions / maxCronExempt /
// maxProjectExempt / maxSysExempt constants (router_core.go) — they touch no
// Router state, so grouping them here keeps the exempt-quota policy in one
// file. Pure code-relocation — no behaviour change.

// exemptKeyPrefixes lists the session-key namespaces that are exempt from
// TTL expiry, LRU eviction, and the active-process counter. Derived from
// keyNamespaces (key.go) so the reserved + exempt lists share a single
// source of truth — see R239-ARCH-L for the prior drift between the two
// independently-maintained slices.
//
// To toggle a namespace's exempt status (or add a new exempt namespace),
// edit the matching `exempt` flag in keyNamespaces in key.go; this slice
// is rebuilt at package init from that table.
//
// Scratch keys are deliberately NOT exempt — they are short-lived and
// should pay the normal TTL / eviction cost (ScratchPool manages its own
// lifetime on top of that). SysKeyPrefix is exempt: system daemon stubs
// (when daemons opt to register one — see docs/rfc/system-session.md)
// must outlive the regular TTL/LRU pressure. Phase 1 daemons typically
// don't register stubs at all (Runner path), but the prefix is reserved
// here to keep the policy consistent with future stub-using daemons.
var exemptKeyPrefixes = func() []string {
	out := make([]string, 0, len(keyNamespaces))
	for _, ns := range keyNamespaces {
		if ns.exempt {
			out = append(out, ns.prefix)
		}
	}
	return out
}()

// exemptInfo scans keyNamespaces once and reports both whether key belongs
// to an exempt namespace and that namespace's kind label. isExemptKey and
// exemptKind are thin wrappers over it so the (bounded, 4-entry) prefix scan
// — including the strings.HasPrefix calls — happens a single time even when
// a caller needs both answers (spawnSession does). R20260603-PERF-8 (#1654):
// merges the two previously-independent scans of the same table.
//
// R239-ARCH-L: derived from keyNamespaces (key.go) so a new exempt namespace
// registers its prefix + kind label in one place.
func exemptInfo(key string) (isExempt bool, kind string) {
	for _, ns := range keyNamespaces {
		if !ns.exempt {
			continue
		}
		if strings.HasPrefix(key, ns.prefix) {
			return true, ns.kind
		}
	}
	return false, ""
}

// isExemptKey reports whether key belongs to an exempt namespace. Callers
// that already have a ManagedSession should prefer reading s.exempt —
// this helper exists for the construction path and for external callers
// that know the key but not the session.
//
// Note: ScratchKeyPrefix is intentionally NOT an exempt namespace — scratch
// sessions are ephemeral and MUST remain subject to the regular TTL /
// eviction policy so an abandoned scratch conversation eventually releases
// its process slot. ScratchPool manages its own lifetime on top of that.
func isExemptKey(key string) bool {
	exempt, _ := exemptInfo(key)
	return exempt
}

// exemptKind classifies an exempt session key into one of three buckets:
// "cron", "project", "sys", or "" if the key is not exempt. Used by the
// per-namespace sub-quota gate in spawnSession so a noisy cron chat
// can't starve planner / sys exempt sessions (R242-ARCH-2).
func exemptKind(key string) string {
	_, kind := exemptInfo(key)
	return kind
}

// exemptCapFor returns the sub-quota cap for a given exempt kind. Unknown
// kinds return maxExemptSessions (the pre-R242-ARCH-2 global cap) so a
// future exempt namespace added to exemptKeyPrefixes without wiring up
// a sub-quota still has a defined limit and never reaches a "missing
// case ⇒ unlimited" state.
func exemptCapFor(kind string) int {
	switch kind {
	case "cron":
		return maxCronExempt
	case "project":
		return maxProjectExempt
	case "sys":
		return maxSysExempt
	default:
		return maxExemptSessions
	}
}
