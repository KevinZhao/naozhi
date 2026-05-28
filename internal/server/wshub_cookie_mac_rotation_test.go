package server

import (
	"testing"
)

// TestHub_CookieMACFn_PropagatesRotation pins R040034-SEC-1 (#1398): the
// Hub's auth-cookie MAC must come from a live getter callback so a future
// RotateCookieGen call invalidates outstanding WS upgrades on the next
// handshake. Previously NewHub cached opts.CookieMAC at construction; the
// HTTP path recomputed cookieMAC() per request but wsDeriveUploadOwner
// compared against the stale cached value, leaving WS as the only surface
// that would keep accepting pre-rotation cookies until the process
// restarted.
//
// The fix: HubOptions.CookieMACFn (preferred) wires a closure; NewHub
// stores it directly and h.cookieMAC() reads the live value on every
// upgrade. We exercise the contract end-to-end by mutating the value
// returned by the closure and asserting the Hub observes the new value
// without a rebuild.
func TestHub_CookieMACFn_PropagatesRotation(t *testing.T) {
	t.Parallel()

	current := "MAC_v1"
	hub := NewHub(HubOptions{
		CookieMACFn: func() string { return current },
	})

	if got := hub.cookieMAC(); got != "MAC_v1" {
		t.Fatalf("initial cookieMAC = %q, want %q — getter callback not wired", got, "MAC_v1")
	}

	// Simulate RotateCookieGen flipping the auth's underlying value.
	// Without the getter contract this assertion would fail because the
	// Hub would still hold the original snapshot.
	current = "MAC_v2"
	if got := hub.cookieMAC(); got != "MAC_v2" {
		t.Fatalf("post-rotation cookieMAC = %q, want %q — Hub cached the value at construction; "+
			"R040034-SEC-1 (#1398) regression: a hot-reload that bumps cookieGenSeq would invalidate "+
			"HTTP cookies but leave WS upgrades accepting the pre-rotation cookie until restart.",
			got, "MAC_v2")
	}
}

// TestHub_CookieMAC_StaticFallback pins the back-compat path: legacy
// callers (existing ws_test.go signature) still pass HubOptions.CookieMAC
// as a static string. NewHub must wrap it in a closure so the rest of the
// Hub uniformly calls cookieMAC() and never has to branch on which option
// the caller picked.
func TestHub_CookieMAC_StaticFallback(t *testing.T) {
	t.Parallel()

	hub := NewHub(HubOptions{
		CookieMAC: "static-mac",
	})

	if got := hub.cookieMAC(); got != "static-mac" {
		t.Fatalf("static fallback cookieMAC = %q, want %q — NewHub must wrap opts.CookieMAC in a closure for the legacy test signature", got, "static-mac")
	}

	// Calling twice still returns the same value (closure captures the
	// constant; this is a pure smoke check that we did not accidentally
	// stash a sync.Once or one-shot value).
	if got := hub.cookieMAC(); got != "static-mac" {
		t.Fatalf("second cookieMAC = %q on legacy static fallback; closure must be idempotent", got)
	}
}

// TestHub_CookieMAC_NilCallback_SafeEmpty pins the nil-safety contract:
// when neither CookieMAC nor CookieMACFn is set, the WS upgrade path's
// `mac != ""` empty-guard must keep working. NewHub installs a closure
// returning "" rather than leaving the field nil so wsDeriveUploadOwner
// does not panic on a typo'd test wiring.
func TestHub_CookieMAC_NilCallback_SafeEmpty(t *testing.T) {
	t.Parallel()

	hub := NewHub(HubOptions{}) // no MAC provided
	if got := hub.cookieMAC(); got != "" {
		t.Fatalf("unset cookieMAC = %q, want empty string — NewHub must install a safe-empty closure even when neither option is set, otherwise wsDeriveUploadOwner panics on the constant-time compare path", got)
	}
}
