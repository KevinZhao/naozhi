package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

func TestConnAdmission_GlobalCapHoldsAndReleases(t *testing.T) {
	a := newConnAdmission(HubOptions{})
	for i := 0; i < maxWSConns; i++ {
		if !a.reserveConn() {
			t.Fatalf("reserve %d refused below the %d cap", i+1, maxWSConns)
		}
	}
	if a.reserveConn() {
		t.Fatalf("reserve past the %d cap was admitted", maxWSConns)
	}
	// A refused reserve gives back what it took: one release is enough to
	// make room again.
	a.releaseConn(1)
	if !a.reserveConn() {
		t.Fatal("no room after releasing one slot: a refused reserve leaked a count")
	}
	if a.reserveConn() {
		t.Fatal("the cap reopened by more than the one released slot")
	}
	a.releaseConn(3)
	for i := 0; i < 3; i++ {
		if !a.reserveConn() {
			t.Fatalf("releaseConn(3) freed only %d slots", i)
		}
	}
}

func TestConnAdmission_UpgradeAndAuthUseSeparateBuckets(t *testing.T) {
	var upgradeIPs []string
	a := newConnAdmission(HubOptions{
		WSAuthLimiter:    func(string) bool { return false },
		WSUpgradeLimiter: func(ip string) bool { upgradeIPs = append(upgradeIPs, ip); return true },
	})
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "203.0.113.5:9000"
	if !a.allowUpgrade(r) {
		t.Fatal("an exhausted auth bucket refused the handshake")
	}
	if len(upgradeIPs) != 1 || upgradeIPs[0] != "203.0.113.5" {
		t.Errorf("upgrade limiter saw %q, want the client IP once", upgradeIPs)
	}
	if a.allowAuthAttempt("203.0.113.5") {
		t.Error("the auth bucket was bypassed")
	}

	// With no upgrade limiter wired the handshake is unlimited; it does not
	// borrow the auth bucket.
	b := newConnAdmission(HubOptions{WSAuthLimiter: func(string) bool { return false }})
	if !b.allowUpgrade(r) {
		t.Error("with no upgrade limiter the handshake fell back to the auth bucket")
	}
	if !newConnAdmission(HubOptions{}).allowAuthAttempt("203.0.113.5") {
		t.Error("with no auth limiter an auth attempt was refused")
	}
}

func TestConnAdmission_TokenMatches(t *testing.T) {
	a := newConnAdmission(HubOptions{DashToken: "secret"})
	if !a.tokenMode() {
		t.Fatal("a configured token did not enable token mode")
	}
	for tok, want := range map[string]bool{"secret": true, "secreT": false, "secret ": false, "": false} {
		if got := a.tokenMatches(tok); got != want {
			t.Errorf("tokenMatches(%q) = %v, want %v", tok, got, want)
		}
	}
	open := newConnAdmission(HubOptions{})
	if open.tokenMode() {
		t.Error("no token configured but token mode is on")
	}
	if open.tokenMatches("") {
		t.Error("with no token configured, an empty token must not count as a match")
	}
}

// TestConnAdmission_CloseDropsPerOwnerState: close releases the owner counts
// and send buckets.
func TestConnAdmission_CloseDropsPerOwnerState(t *testing.T) {
	a := newConnAdmission(HubOptions{})
	for i := 0; i < maxConnsPerOwner; i++ {
		a.reserveOwner("A")
	}
	for i := 0; i < 10; i++ {
		a.allowSend("A")
	}
	a.close()

	a.ownersMu.Lock()
	n := len(a.owners)
	a.ownersMu.Unlock()
	if n != 0 {
		t.Errorf("owners holds %d entries after close", n)
	}
	var buckets int
	a.sendLimiters.Range(func(any, any) bool { buckets++; return true })
	if buckets != 0 {
		t.Errorf("sendLimiters holds %d buckets after close", buckets)
	}
}

// TestConnAdmission_RekeyRefusedKeepsTheOldSlot: a re-key refused at the new
// owner's cap leaves the client on its old owner with that slot still held,
// so the release at teardown stays paired.
func TestConnAdmission_RekeyRefusedKeepsTheOldSlot(t *testing.T) {
	a := newConnAdmission(HubOptions{})
	c := &wsClient{done: make(chan struct{})}
	c.setUploadOwner("old")
	a.reserveOwner("old")
	for i := 0; i < maxConnsPerOwner; i++ {
		a.reserveOwner("new")
	}
	if a.rekeyOwner(c, "old", "new") {
		t.Fatal("re-key onto an owner at its cap succeeded")
	}
	if got := c.uploadOwnerKey(); got != "old" {
		t.Errorf("owner = %q after a refused re-key, want old", got)
	}
	a.ownersMu.Lock()
	oldN, newN := a.owners["old"], a.owners["new"]
	a.ownersMu.Unlock()
	if oldN != 1 || newN != maxConnsPerOwner {
		t.Errorf("slots old=%d new=%d after a refused re-key, want 1 and %d", oldN, newN, maxConnsPerOwner)
	}
}

// TestHandleUpgrade_RejectsCrossOriginHandshake: a page on another origin
// cannot open a WebSocket to the dashboard with the user's cookies.
func TestHandleUpgrade_RejectsCrossOriginHandshake(t *testing.T) {
	hub, _ := newTestHub("")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()
	host := strings.TrimPrefix(strings.TrimSuffix(url, "/ws"), "ws://")

	conn, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": {"http://evil.example"}})
	if err == nil {
		conn.Close()
		t.Fatal("cross-origin handshake was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin handshake: resp=%v err=%v, want 403", resp, err)
	}

	conn, _, err = websocket.DefaultDialer.Dial(url, http.Header{"Origin": {"http://" + host}})
	if err != nil {
		t.Fatalf("same-origin handshake refused: %v", err)
	}
	conn.Close()
}

// TestDeriveUploadOwner_EmptyCookieMACAuthenticatesNothing: in token mode
// with no cookie MAC available, an auth cookie (even an empty one) must not
// pre-authenticate the connection.
func TestDeriveUploadOwner_EmptyCookieMACAuthenticatesNothing(t *testing.T) {
	a := newConnAdmission(HubOptions{DashToken: "secret"})
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Header.Set("Cookie", auth.AuthCookieName+"=")
	if _, err := r.Cookie(auth.AuthCookieName); err != nil {
		t.Fatalf("test setup: empty auth cookie not parsed: %v", err)
	}
	owner, authed, ok := a.deriveUploadOwner(httptest.NewRecorder(), r, "10.0.0.1")
	if !ok || authed || owner != "" {
		t.Errorf("empty MAC + empty cookie: owner=%q authed=%v ok=%v, want unauthenticated", owner, authed, ok)
	}
}
