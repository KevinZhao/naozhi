package node

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
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
