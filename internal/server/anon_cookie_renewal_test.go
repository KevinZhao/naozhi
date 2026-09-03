package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/session"
)

// These tests pin the no-token upload-owner divergence fix ("本机不能添加
// 图片附件" bug). Failure chain being closed:
//
//  1. nz_anon hard-expired anonCookieMaxAgeSeconds (1h) after mint — #2297
//     aligned the MaxAge down from 7d but skipped the sliding renewal that
//     nz_auth already has, so an actively-used dashboard lost its label.
//  2. The WS connection's uploadOwner is frozen at upgrade time and never
//     refreshed in no-token mode, so after the label expired the next
//     /api/sessions/upload minted a NEW label → upload owner ≠ WS owner →
//     every file-bearing WS send failed TakeAll with "file not found or
//     expired".
//  3. Worse, a label minted ON the WS handshake never reached the browser
//     at all: gorilla's Upgrade hijacks the response and drops w.Header()
//     entries unless they are passed via the responseHeader parameter, and
//     HandleUpgrade passed nil.
//
// Fixes under test: sliding renewal on the HTTP owner-derivation path and
// the WS handshake, plus forwarding Set-Cookie into the 101 response.

const testAnonVal = "deadbeefcafebabe0011223344556677"

// TestUploadOwner_SlidingRenewal: presenting a valid nz_anon over HTTP must
// (a) derive the owner from it unchanged, and (b) re-issue the SAME value
// with a fresh MaxAge so active use keeps the label alive.
func TestUploadOwner_SlidingRenewal(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/upload", nil)
	r.AddCookie(&http.Cookie{Name: anonCookieName, Value: testAnonVal})
	w := httptest.NewRecorder()

	owner, ok := uploadOwner(w, r, &auth.Handlers{}, false)
	if !ok {
		t.Fatal("uploadOwner ok=false; want true for valid nz_anon")
	}
	if want := ownerKeyFromCookie(testAnonVal); owner != want {
		t.Fatalf("owner=%q; want %q — renewal must NOT rotate the owner bucket", owner, want)
	}
	var renewed bool
	for _, c := range w.Result().Cookies() {
		if c.Name != anonCookieName {
			continue
		}
		if c.Value != testAnonVal {
			t.Fatalf("renewal changed the cookie value to %q — owner bucket would rotate mid-session", c.Value)
		}
		if c.MaxAge != anonCookieMaxAgeSeconds {
			t.Fatalf("renewal MaxAge=%d; want %d", c.MaxAge, anonCookieMaxAgeSeconds)
		}
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("renewal dropped hardening attributes: %#v", c)
		}
		renewed = true
	}
	if !renewed {
		t.Fatalf("no Set-Cookie renewal on valid nz_anon — label hard-expires after %ds of active use and the upload owner diverges from the frozen WS owner", anonCookieMaxAgeSeconds)
	}
}

// TestUploadOwner_InvalidCookieStillMintsFresh: renewal must not weaken the
// R236-SEC-06 shape gate — a malformed nz_anon is replaced, never renewed.
func TestUploadOwner_InvalidCookieStillMintsFresh(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/upload", nil)
	r.AddCookie(&http.Cookie{Name: anonCookieName, Value: "NOT-HEX-ATTACKER-CONTROLLED!!"})
	w := httptest.NewRecorder()

	owner, ok := uploadOwner(w, r, &auth.Handlers{}, false)
	if !ok || owner == "" {
		t.Fatalf("uploadOwner ok=%v owner=%q; want fresh mint", ok, owner)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == anonCookieName && c.Value == "NOT-HEX-ATTACKER-CONTROLLED!!" {
			t.Fatal("attacker-shaped cookie value was renewed verbatim — must be replaced by a server-minted value")
		}
	}
}

func newAnonTestHub(t *testing.T) *Hub {
	t.Helper()
	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	return NewHub(HubOptions{
		Router: router, DashToken: "", Guard: guard,
		Auth: &auth.Handlers{},
	})
}

// TestHandleUpgrade_SetCookieRidesThe101 pins the responseHeader fix: gorilla
// Upgrade hijacks the connection and silently DROPS headers written to w
// unless they are forwarded via its responseHeader argument. Both the mint
// path (no cookie presented) and the renewal path (valid cookie presented)
// must surface Set-Cookie on the actual 101 handshake response the browser
// sees — not just on the recorder in a unit test.
func TestHandleUpgrade_SetCookieRidesThe101(t *testing.T) {
	hub := newAnonTestHub(t)
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	anonFrom := func(resp *http.Response) *http.Cookie {
		for _, c := range resp.Cookies() {
			if c.Name == anonCookieName {
				return c
			}
		}
		return nil
	}

	t.Run("mint path", func(t *testing.T) {
		conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		c := anonFrom(resp)
		if c == nil {
			t.Fatal("101 response carries no nz_anon Set-Cookie — Upgrade(w, r, nil) dropped the minted cookie; the browser-side label and the server-side owner diverge immediately")
		}
		if !isValidAnonCookieValue(c.Value) {
			t.Fatalf("minted cookie value %q is not the expected 32-hex shape", c.Value)
		}
	})

	t.Run("renewal path", func(t *testing.T) {
		hdr := http.Header{}
		hdr.Set("Cookie", anonCookieName+"="+testAnonVal)
		conn, resp, err := websocket.DefaultDialer.Dial(url, hdr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		c := anonFrom(resp)
		if c == nil {
			t.Fatal("101 response carries no nz_anon renewal — label hard-expires under the live connection")
		}
		if c.Value != testAnonVal {
			t.Fatalf("renewal on 101 changed the value to %q; must re-issue the presented value", c.Value)
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Set-Cookie")), "max-age=") {
			t.Fatalf("renewal Set-Cookie missing Max-Age: %q", resp.Header.Get("Set-Cookie"))
		}
	})
}
