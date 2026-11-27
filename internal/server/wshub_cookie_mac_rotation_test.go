package server

import (
	"testing"
)

// TestHub_CookieMACFn_PropagatesRotation pins R040034-SEC-1 (#1398): the
// Hub's auth-cookie MAC must come from a live getter callback so a future
// RotateCookieGen call invalidates outstanding WS upgrades on the next
// handshake. Previously NewHub cached opts.CookieMAC at construction; the
// HTTP path recomputed cookieMAC() per request but deriveUploadOwner
// compared against the stale cached value, leaving WS as the only surface
// that would keep accepting pre-rotation cookies until the process
// restarted.
//
// The fix: HubOptions.CookieMACFn (preferred) wires a closure; NewHub
// stores it directly and admit.cookieMAC() reads the live value on every
// upgrade. We exercise the contract end-to-end by mutating the value
// returned by the closure and asserting the Hub observes the new value
// without a rebuild.
func TestHub_CookieMACFn_PropagatesRotation(t *testing.T) {
	t.Parallel()

	current := "MAC_v1"
	hub := newHubForTest(t, HubOptions{
		CookieMACFn: func() string { return current },
	}, sendEngineOpts{})

	if got := hub.admit.cookieMAC(); got != "MAC_v1" {
		t.Fatalf("initial cookieMAC = %q, want %q — getter callback not wired", got, "MAC_v1")
	}

	// Simulate RotateCookieGen flipping the auth's underlying value.
	// Without the getter contract this assertion would fail because the
	// Hub would still hold the original snapshot.
	current = "MAC_v2"
	if got := hub.admit.cookieMAC(); got != "MAC_v2" {
		t.Fatalf("post-rotation cookieMAC = %q, want %q — Hub cached the value at construction; "+
			"R040034-SEC-1 (#1398) regression: a hot-reload that bumps cookieGenSeq would invalidate "+
			"HTTP cookies but leave WS upgrades accepting the pre-rotation cookie until restart.",
			got, "MAC_v2")
	}
}
