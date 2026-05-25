package session

import (
	"testing"
)

// TestKeyNamespaces_DriveDerivedSlices pins the R239-ARCH-L invariant:
// keyNamespaces is the single source of truth for both reservedKeyPrefixes
// (used by IsReservedNamespace / IsUserVisibleKey) and exemptKeyPrefixes
// (used by isExemptKey / exemptKind / exemptCapFor).
//
// The two slices used to live in separate files and drift without warning;
// the contract here asserts they are now derived consistently from
// keyNamespaces and that the derivation respects the exempt flag.
func TestKeyNamespaces_DriveDerivedSlices(t *testing.T) {
	t.Parallel()

	// reservedKeyPrefixes must be exactly the prefixes in keyNamespaces, in
	// the same order. Order matters because IsReservedNamespace iterates
	// the slice in declaration order — a future caller using strings.HasPrefix
	// behaves identically regardless of order, but downstream tooling that
	// snapshots the list (e.g. doc generators) relies on stable ordering.
	if got, want := len(reservedKeyPrefixes), len(keyNamespaces); got != want {
		t.Fatalf("reservedKeyPrefixes len=%d, keyNamespaces len=%d; "+
			"derivation lost or grew an entry", got, want)
	}
	for i, ns := range keyNamespaces {
		if reservedKeyPrefixes[i] != ns.prefix {
			t.Errorf("reservedKeyPrefixes[%d]=%q, keyNamespaces[%d].prefix=%q; "+
				"order drifted", i, reservedKeyPrefixes[i], i, ns.prefix)
		}
	}

	// exemptKeyPrefixes must be exactly { ns.prefix | ns ∈ keyNamespaces, ns.exempt }.
	// We derive the expected slice in the same iteration order the
	// production code uses so a future field reorder fails loudly here.
	var wantExempt []string
	for _, ns := range keyNamespaces {
		if ns.exempt {
			wantExempt = append(wantExempt, ns.prefix)
		}
	}
	if got, want := len(exemptKeyPrefixes), len(wantExempt); got != want {
		t.Fatalf("exemptKeyPrefixes len=%d, derived-from-keyNamespaces len=%d; "+
			"derivation diverged", got, want)
	}
	for i, p := range wantExempt {
		if exemptKeyPrefixes[i] != p {
			t.Errorf("exemptKeyPrefixes[%d]=%q, want %q (derived from keyNamespaces)",
				i, exemptKeyPrefixes[i], p)
		}
	}

	// Negative: every entry in exemptKeyPrefixes must appear in
	// reservedKeyPrefixes (exempt ⊂ reserved). A namespace can't be
	// "exempt from TTL" without being a reserved non-IM-shape — IM
	// keys go through the standard chat key router, never the exempt
	// path.
	reservedSet := make(map[string]bool, len(reservedKeyPrefixes))
	for _, p := range reservedKeyPrefixes {
		reservedSet[p] = true
	}
	for _, p := range exemptKeyPrefixes {
		if !reservedSet[p] {
			t.Errorf("exemptKeyPrefixes contains %q but it is not in "+
				"reservedKeyPrefixes; an exempt namespace must also be a "+
				"reserved (non-IM-shape) one — otherwise the IM session "+
				"router's standard chat-key path would never reach the "+
				"exempt classification.", p)
		}
	}

	// Symmetric check: ScratchKeyPrefix is the canonical reserved-but-not-
	// exempt entry. If a future change makes scratch exempt, that's a
	// significant policy shift (scratch becomes long-lived) and must
	// route through this test plus DESIGN.md.
	scratchExempt := false
	for _, ns := range keyNamespaces {
		if ns.prefix == ScratchKeyPrefix {
			scratchExempt = ns.exempt
			break
		}
	}
	if scratchExempt {
		t.Error("ScratchKeyPrefix is now exempt in keyNamespaces. R239-ARCH-L: " +
			"scratch sessions are deliberately non-exempt so abandoned " +
			"scratch conversations release process slots via the regular " +
			"TTL eviction. If exempting scratch is intentional, update " +
			"DESIGN.md §\"Session key namespace\", ScratchPool's lifetime " +
			"contract, and remove this assertion in the same patch.")
	}
}

// TestExemptKindCovers_AllExempt asserts that exemptKind(key) returns a
// non-empty bucket for every prefix in exemptKeyPrefixes. A namespace
// gaining exempt status without a matching exemptKind/exemptCapFor case
// silently routes through the maxExemptSessions fallback and bypasses
// per-namespace sub-quota isolation (R242-ARCH-2).
func TestExemptKindCovers_AllExempt(t *testing.T) {
	t.Parallel()
	for _, prefix := range exemptKeyPrefixes {
		// Synthesise a key in the namespace; the suffix value is irrelevant
		// because exemptKind only checks HasPrefix.
		key := prefix + "test-id"
		kind := exemptKind(key)
		if kind == "" {
			t.Errorf("exemptKind(%q) = \"\" for an exempt namespace; "+
				"add a switch case in router_core.go::exemptKind so the "+
				"per-namespace sub-quota cap (exemptCapFor) applies.", key)
		}
		if cap := exemptCapFor(kind); cap <= 0 {
			t.Errorf("exemptCapFor(%q) = %d (≤0) for kind derived from "+
				"prefix %q; sub-quota must be positive.", kind, cap, prefix)
		}
	}
}
