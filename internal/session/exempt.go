package session

import "strings"

// File: exempt.go
//
// Stateless exempt-namespace quota helpers: pure functions over the
// keyNamespaces table (key.go) and the max*Exempt constants
// (router_core.go). They touch no Router state.

// exemptKeyPrefixes lists the session-key namespaces that are exempt from
// TTL expiry, LRU eviction, and the active-process counter. Rebuilt at
// package init from keyNamespaces (key.go) so the reserved + exempt lists
// share a single source of truth; toggle the `exempt` flag there.
//
// Scratch keys are deliberately NOT exempt — they must pay the normal TTL /
// eviction cost (ScratchPool manages its own lifetime on top). SysKeyPrefix
// is exempt so system daemon stubs outlive TTL/LRU pressure
// (docs/rfc/system-session.md).
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
// to an exempt namespace and that namespace's kind label, so callers that
// need both answers (spawnSession) pay for a single prefix scan.
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
// that already have a ManagedSession should prefer reading s.exempt; this
// exists for the construction path and callers that only know the key.
func isExemptKey(key string) bool {
	exempt, _ := exemptInfo(key)
	return exempt
}

// exemptKind classifies an exempt session key as "cron", "project", "sys",
// or "" if the key is not exempt. Drives the per-namespace sub-quota gate
// in spawnSession so a noisy cron chat can't starve planner / sys sessions.
func exemptKind(key string) string {
	_, kind := exemptInfo(key)
	return kind
}

// exemptCapFor returns the sub-quota cap for an exempt kind. Unknown kinds
// fall back to maxExemptSessions so a new exempt namespace without a wired
// sub-quota still has a defined limit rather than "missing case ⇒ unlimited".
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
