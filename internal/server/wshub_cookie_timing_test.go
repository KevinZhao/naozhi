package server

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// TestWsDeriveUploadOwner_TimingSafeCompare pins R20260531A-SEC-11: the
// cookie MAC comparison in wsDeriveUploadOwner must hash both sides to a
// fixed 32-byte digest before calling subtle.ConstantTimeCompare. Without
// the hash step, different-length inputs cause an immediate 0 return from
// ConstantTimeCompare, leaking the expected MAC length via timing.
//
// This test exercises two properties:
//  1. A cookie value that is the correct MAC but has a different length
//     prefix/suffix must NOT authenticate (correctness unchanged by fix).
//  2. The valid-MAC path still authenticates after the hash step
//     (regression: the pre-hash must use the original value, not the digest).
func TestWsDeriveUploadOwner_TimingSafeCompare(t *testing.T) {
	t.Parallel()

	const token = "test-token-timing-sec11"
	const secret = "timing-secret-abcdefgh"
	const gen = "timing-gen-v1"

	ah := helperMakeAuth(token, secret, gen)
	validMAC := ah.CookieMAC()
	if validMAC == "" {
		t.Fatal("test setup: helperMakeAuth returned empty CookieMAC — check token/secret/gen")
	}

	hub := NewHub(HubOptions{
		DashToken:   token,
		CookieMACFn: ah.CookieMAC,
	})

	hitUpgrade := func(cookieValue string) (owner string, authenticated bool) {
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.Header.Set("Connection", "upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.RemoteAddr = "127.0.0.1:9999"
		if cookieValue != "" {
			req.AddCookie(&http.Cookie{
				Name:  auth.AuthCookieName,
				Value: cookieValue,
			})
		}
		w := httptest.NewRecorder()
		o, authed, _ := wsDeriveUploadOwner(w, req, hub, "127.0.0.1")
		return o, authed
	}

	t.Run("valid_mac_authenticates", func(t *testing.T) {
		t.Parallel()
		owner, authed := hitUpgrade(validMAC)
		if !authed {
			t.Fatalf("valid MAC must authenticate; owner=%q — hash-before-compare broke the legitimate path", owner)
		}
		want := ownerKeyFromCookie(validMAC)
		if owner != want {
			t.Fatalf("owner = %q, want %q", owner, want)
		}
	})

	t.Run("shorter_prefix_rejected", func(t *testing.T) {
		t.Parallel()
		// A prefix of the valid MAC is shorter — without pre-hashing,
		// ConstantTimeCompare would immediately return 0 on length mismatch.
		// With pre-hashing, both sides become 32 bytes so the comparison
		// always runs to completion and correctly rejects the mismatch.
		prefix := validMAC[:len(validMAC)/2]
		_, authed := hitUpgrade(prefix)
		if authed {
			t.Fatal("MAC prefix must NOT authenticate")
		}
	})

	t.Run("longer_padded_rejected", func(t *testing.T) {
		t.Parallel()
		// A value longer than the valid MAC — previously the length
		// difference would cause an immediate 0 return.
		longer := validMAC + "xxxxxxxxxxxxxx"
		_, authed := hitUpgrade(longer)
		if authed {
			t.Fatal("padded MAC must NOT authenticate")
		}
	})

	t.Run("hash_of_valid_mac_rejected", func(t *testing.T) {
		// The fix hashes cookie.Value before comparing against sha256(mac).
		// Supplying sha256(validMAC) as the cookie value must NOT match —
		// that would mean the compare is sha256(sha256(mac)) == sha256(mac).
		t.Parallel()
		h := sha256.Sum256([]byte(validMAC))
		hashHex := make([]byte, 64)
		const hextable = "0123456789abcdef"
		for i, b := range h {
			hashHex[i*2] = hextable[b>>4]
			hashHex[i*2+1] = hextable[b&0xf]
		}
		_, authed := hitUpgrade(string(hashHex))
		if authed {
			t.Fatal("sha256(validMAC) as cookie value must NOT authenticate — pre-hash is of the raw value, not a re-hash")
		}
	})
}
