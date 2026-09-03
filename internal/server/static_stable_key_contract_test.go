package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_ResolveSessionKeyWiring pins the project-stable-session-key
// frontend wiring (RFC docs/rfc/project-stable-session-key.md §4.4). dashboard.js
// is not exercised by a JS unit runner, so this Go contract guards the key
// invariants against a silent refactor:
//
//  1. resolveSessionKey exists and is the single key-derivation entry for
//     project opens (continue vs new).
//  2. The continue path reuses a backend dashboard:pj: key and only swaps
//     the trailing agent segment (no client-side sha256 → no algorithm drift).
//  3. The "New Session" palette project row starts a FRESH session
//     (mode:'new', no stableKey). Continuing the project-stable
//     conversation is the sidebar card's job; stats.projects only started
//     carrying stableKey in v0.0.78, and when it did the row silently began
//     resuming the folder's running session (#2476).
//  4. Quick sessions stay one-off (mode:'new').
func TestDashboardJS_ResolveSessionKeyWiring(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// The pure resolver exists.
		"function resolveSessionKey(mode, stableKey, projectOrFolder, agentID, timestamp)",
		// Continue path reconstructs the pj key from the backend hash segment.
		"'dashboard:pj:' + parts[2] + ':' + agent",
		// Palette project row starts a fresh session (#2476).
		"{ mode: 'new', accessProfile: accessProfile }",
		// Quick session is explicitly one-off.
		"{ mode: 'new' }",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing project-stable-key wiring: %q", want)
		}
	}
	// The palette must not resume via stableKey any more: New Session means new.
	if strings.Contains(js, "{ mode: 'continue', stableKey: p.stableKey") {
		t.Errorf("pickPaletteProject still continues the project-stable session; the New Session palette row must use mode:'new' (#2476)")
	}

	// Guard: the continue branch must NOT compute a hash client-side. A
	// crypto/subtle or sha256 reference near key derivation would reintroduce
	// the front/back algorithm-drift risk the RFC §4.2 decision avoids.
	if strings.Contains(js, "crypto.subtle.digest") || strings.Contains(js, "sha256(") {
		t.Errorf("dashboard.js appears to hash workspace paths client-side; the backend must own ProjectStableKey (RFC §4.2)")
	}
}
