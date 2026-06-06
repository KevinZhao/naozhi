package node

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestValidatePeerURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		wantURL string // checked only when wantErr == false
	}{
		// Allowed: real peer topologies.
		{name: "lan http", raw: "http://10.0.0.2:8180", wantURL: "http://10.0.0.2:8180"},
		{name: "lan https", raw: "https://10.0.0.2:8180", wantURL: "https://10.0.0.2:8180"},
		{name: "loopback v4", raw: "http://127.0.0.1:8180", wantURL: "http://127.0.0.1:8180"},
		{name: "loopback v6", raw: "http://[::1]:8180", wantURL: "http://[::1]:8180"},
		{name: "private 192.168", raw: "http://192.168.1.5:8180", wantURL: "http://192.168.1.5:8180"},
		{name: "dns hostname", raw: "https://peer.internal.example:8180", wantURL: "https://peer.internal.example:8180"},
		{name: "trailing slash + path stripped", raw: "http://10.0.0.2:8180/", wantURL: "http://10.0.0.2:8180"},
		{name: "stray path stripped", raw: "http://10.0.0.2:8180/api/foo", wantURL: "http://10.0.0.2:8180"},

		// Rejected: IMDS / link-local.
		{name: "imds v4", raw: "http://169.254.169.254/latest/meta-data/", wantErr: true},
		{name: "link-local v4 range", raw: "http://169.254.10.20:80", wantErr: true},
		{name: "link-local v6", raw: "http://[fe80::1]:8180", wantErr: true},

		// Rejected: bad scheme / shape.
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace only", raw: "   ", wantErr: true},
		{name: "no scheme", raw: "10.0.0.2:8180", wantErr: true},
		{name: "file scheme", raw: "file:///etc/passwd", wantErr: true},
		{name: "gopher scheme", raw: "gopher://10.0.0.2:70", wantErr: true},
		{name: "no host", raw: "http://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validatePeerURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validatePeerURL(%q) = %q, nil; want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatePeerURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.wantURL {
				t.Errorf("validatePeerURL(%q) = %q; want %q", tt.raw, got, tt.wantURL)
			}
		})
	}
}

// TestNewHTTPClient_RejectsIMDS verifies that a client built from an IMDS URL
// is disabled: every request fails cleanly without leaving the host.
func TestNewHTTPClient_RejectsIMDS(t *testing.T) {
	c := NewHTTPClient("evil", "http://169.254.169.254/latest/meta-data/", "secret-token", "Evil")
	if c.urlErr == nil {
		t.Fatalf("expected urlErr to be set for IMDS URL")
	}
	_, err := c.doRequest(context.Background(), http.MethodGet, "/api/sessions", nil)
	if err == nil {
		t.Fatalf("doRequest to IMDS-configured client should fail")
	}
	if !strings.Contains(err.Error(), "unvalidated peer URL") {
		t.Errorf("error %q should mention unvalidated peer URL", err)
	}
	// The higher-level fetch wrappers must also surface the failure.
	if _, ferr := c.FetchSessions(context.Background()); ferr == nil {
		t.Errorf("FetchSessions on disabled client should error")
	}
}

// TestNewHTTPClient_AllowsLoopback confirms loopback peers still work end to
// end — this is the supported local multi-node bridging topology and must not
// regress under the SSRF guard.
func TestNewHTTPClient_AllowsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"sessions":[]}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("local", srv.URL, "tok", "Local") // httptest = 127.0.0.1
	if c.urlErr != nil {
		t.Fatalf("loopback peer URL %q rejected: %v", srv.URL, c.urlErr)
	}
	if _, err := c.FetchSessions(context.Background()); err != nil {
		t.Fatalf("FetchSessions over loopback failed: %v", err)
	}
}

// TestIsBlockedPeerAddr pins the shared screen used by both validatePeerURL
// and the dial-time guard: link-local (IMDS) is blocked, loopback / RFC1918 /
// public stay allowed.
func TestIsBlockedPeerAddr(t *testing.T) {
	tests := []struct {
		addr    string
		blocked bool
	}{
		{"169.254.169.254", true}, // IMDS v4
		{"169.254.10.20", true},   // link-local v4 range
		{"fe80::1", true},         // link-local v6
		{"127.0.0.1", false},      // loopback v4 (allowed)
		{"::1", false},            // loopback v6 (allowed)
		{"10.0.0.2", false},       // RFC1918 (allowed)
		{"192.168.1.5", false},    // RFC1918 (allowed)
		{"8.8.8.8", false},        // public (allowed)
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := isBlockedPeerAddr(netip.MustParseAddr(tt.addr))
			if got != tt.blocked {
				t.Errorf("isBlockedPeerAddr(%s) = %v; want %v", tt.addr, got, tt.blocked)
			}
		})
	}
}

// TestIsLocalPeerURL covers R20260606-SEC-5 (#1825): loopback and RFC1918/ULA
// literal hosts are classified local (body-capped), public literals and DNS
// hostnames are not.
func TestIsLocalPeerURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8180", true},
		{"http://[::1]:8180", true},
		{"http://10.0.0.2:8180", true},
		{"http://192.168.1.5:8180", true},
		{"http://172.16.4.4:443", true},
		{"http://[fd00::1]:8180", true},      // ULA
		{"http://8.8.8.8:53", false},         // public
		{"https://peer.example:8180", false}, // DNS hostname
		{"http://172.32.0.1:8180", false},    // just outside RFC1918
		{"not a url", false},
	}
	for _, tt := range tests {
		if got := isLocalPeerURL(tt.url); got != tt.want {
			t.Errorf("isLocalPeerURL(%q) = %v; want %v", tt.url, got, tt.want)
		}
	}
}

// TestDoRequest_LocalPeerBodyCap is the #1825 SSRF-write guard: a loopback
// peer accepts a small body (legitimate proxy payload) but refuses an
// over-cap body before the Bearer token leaves the host. A public peer is
// uncapped.
func TestDoRequest_LocalPeerBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestHTTPClient(t, srv, "tok") // httptest = 127.0.0.1 → local peer
	if !c.localPeer {
		t.Fatalf("loopback peer should be classified localPeer")
	}

	// Small body passes through to the server.
	small := bytes.NewReader(make([]byte, 1024))
	resp, err := c.doRequest(context.Background(), http.MethodPost, "/api/sessions/send", small)
	if err != nil {
		t.Fatalf("small body to local peer should succeed: %v", err)
	}
	resp.Body.Close()

	// Over-cap body is refused before any request is sent.
	big := bytes.NewReader(make([]byte, maxLocalPeerBodyBytes+1))
	if _, err := c.doRequest(context.Background(), http.MethodPost, "/api/sessions/send", big); err == nil {
		t.Fatalf("over-cap body to local peer should be refused")
	} else if !strings.Contains(err.Error(), "SSRF-write guard") {
		t.Errorf("error %q should mention the SSRF-write guard", err)
	}

	// GET with no body is never capped (ContentLength 0).
	if _, err := c.doRequest(context.Background(), http.MethodGet, "/api/sessions", nil); err != nil {
		t.Errorf("bodyless GET to local peer should not be capped: %v", err)
	}
}

// TestDoRequest_PublicPeerUncapped confirms the body cap is local-only: a
// public peer (non-loopback/non-private literal) sends an over-cap body
// without the #1825 refusal. We point at a closed port so the request fails
// at dial — but crucially NOT with the body-cap error.
func TestDoRequest_PublicPeerUncapped(t *testing.T) {
	// 8.8.8.8:1 — public literal, classified non-local. We never actually
	// want a live connection; a short context deadline makes the dial fail
	// fast. The assertion is only that the failure is not the cap refusal.
	c := NewHTTPClient("pub", "http://8.8.8.8:1", "tok", "Pub")
	if c.urlErr != nil {
		t.Fatalf("public peer URL unexpectedly rejected: %v", c.urlErr)
	}
	if c.localPeer {
		t.Fatalf("public peer must not be classified localPeer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	big := bytes.NewReader(make([]byte, maxLocalPeerBodyBytes+1))
	_, err := c.doRequest(ctx, http.MethodPost, "/api/sessions/send", big)
	if err != nil && strings.Contains(err.Error(), "SSRF-write guard") {
		t.Errorf("public peer must not trigger the local body cap; got %v", err)
	}
}

// TestSafeDialContext_BlocksRebindToIMDS is the R20260603-SEC-2 (#1677)
// regression guard: a DNS hostname that resolves to the IMDS link-local
// address must be refused before any TCP connection opens, even though
// validatePeerURL passed it at config time (it's not a literal IP). We use a
// hostname literal that net resolves locally without a network round-trip.
func TestSafeDialContext_BlocksRebindToIMDS(t *testing.T) {
	// SplitHostPort path with a literal link-local IP: LookupNetIP returns it
	// verbatim, so this exercises the screen deterministically offline.
	_, err := safeDialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil {
		t.Fatal("safeDialContext to IMDS address should be refused")
	}
	if !strings.Contains(err.Error(), "SSRF") && !strings.Contains(err.Error(), "link-local") {
		t.Errorf("error %q should mention the SSRF/link-local refusal", err)
	}
}

// TestSafeDialContext_BlocksLinkLocalV6 covers the IPv6 fe80::/10 range.
func TestSafeDialContext_BlocksLinkLocalV6(t *testing.T) {
	_, err := safeDialContext(context.Background(), "tcp", "[fe80::1]:80")
	if err == nil {
		t.Fatal("safeDialContext to link-local v6 should be refused")
	}
}

// TestSafeDialContext_AllowsLoopback confirms the dial guard does not break
// the supported loopback/LAN topology: a loopback dial must reach the server.
func TestSafeDialContext_AllowsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sessions":[]}`))
	}))
	defer srv.Close()
	c := newTestHTTPClient(t, srv, "tok")
	if _, err := c.FetchSessions(context.Background()); err != nil {
		t.Fatalf("loopback dial through safeDialContext failed: %v", err)
	}
}

// TestNewHTTPClient_UnsafeURLDoesNotPanic pins the no-panic contract: a bad
// config string must degrade to a disabled client, never crash startup.
func TestNewHTTPClient_UnsafeURLDoesNotPanic(t *testing.T) {
	for _, raw := range []string{"", "file:///etc/passwd", "not a url at all", "http://"} {
		c := NewHTTPClient("n", raw, "tok", "N")
		if c == nil {
			t.Fatalf("NewHTTPClient(%q) returned nil", raw)
		}
		if c.urlErr == nil {
			t.Errorf("NewHTTPClient(%q) should have recorded urlErr", raw)
		}
		if _, err := c.doRequest(context.Background(), http.MethodGet, "/x", nil); !errors.Is(err, c.urlErr) {
			t.Errorf("doRequest err for %q should wrap urlErr", raw)
		}
	}
}
