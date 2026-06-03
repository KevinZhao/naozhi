package weixin

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// dialContextOf returns the SSRF DialContext installed on an apiClient's
// transport, or nil when the transport uses the default dialer.
func dialContextOf(c *apiClient) func(context.Context, string, string) (net.Conn, error) {
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		return nil
	}
	return tr.DialContext
}

// R20260603040203-SEC-10: rejectInternalIP must refuse the SSRF deny-set
// (loopback / private / link-local / IMDS / unspecified) and pass public IPs.
func TestRejectInternalIP(t *testing.T) {
	t.Parallel()
	deny := []string{
		"127.0.0.1",       // loopback
		"::1",             // loopback v6
		"10.0.0.1",        // RFC1918
		"192.168.1.1",     // RFC1918
		"172.16.0.1",      // RFC1918
		"169.254.169.254", // EC2 IMDS / link-local
		"fc00::1",         // ULA private v6
		"fe80::1",         // link-local v6
		"0.0.0.0",         // unspecified
	}
	for _, s := range deny {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if err := rejectInternalIP(ip); err == nil {
			t.Errorf("rejectInternalIP(%s) = nil; expected refusal", s)
		} else if !strings.Contains(err.Error(), "SSRF") {
			t.Errorf("rejectInternalIP(%s) error should mention SSRF, got %q", s, err.Error())
		}
	}

	allow := []string{"8.8.8.8", "1.1.1.1", "203.0.113.10", "2606:4700::1111"}
	for _, s := range allow {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if err := rejectInternalIP(ip); err != nil {
			t.Errorf("rejectInternalIP(%s) = %v; expected nil for public IP", s, err)
		}
	}
}

// The dial guard must block a literal IMDS address before the base dialer is
// ever invoked (so no connection is attempted to the internal target).
func TestSSRFDialGuard_BlocksLiteralIMDS(t *testing.T) {
	t.Parallel()
	var baseCalled bool
	base := func(_ context.Context, network, addr string) (net.Conn, error) {
		baseCalled = true
		return nil, nil
	}
	guarded := ssrfDialGuard(base)
	_, err := guarded(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil {
		t.Fatal("expected SSRF refusal dialing 169.254.169.254")
	}
	if baseCalled {
		t.Error("base dialer must NOT be called when target is internal")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("error should mention SSRF, got %q", err.Error())
	}
}

// A literal public IP must pass through to the base dialer.
func TestSSRFDialGuard_AllowsLiteralPublicIP(t *testing.T) {
	t.Parallel()
	var gotAddr string
	base := func(_ context.Context, network, addr string) (net.Conn, error) {
		gotAddr = addr
		return nil, nil
	}
	guarded := ssrfDialGuard(base)
	if _, err := guarded(context.Background(), "tcp", "8.8.8.8:443"); err != nil {
		t.Fatalf("public IP should pass, got %v", err)
	}
	if gotAddr != "8.8.8.8:443" {
		t.Errorf("base dialer addr = %q, want 8.8.8.8:443", gotAddr)
	}
}

func TestIsLoopbackBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		want bool
	}{
		{"", false}, // empty → public default
		{"https://ilinkai.weixin.qq.com", false},
		{"http://127.0.0.1:9000", true},
		{"http://localhost:8080", true},
		{"http://[::1]:9000", true},
		{"http://169.254.169.254", false},
		{"http://10.0.0.5", false},
		{"https://wechat.example.com", false},
	}
	for _, c := range cases {
		if got := isLoopbackBaseURL(c.url); got != c.want {
			t.Errorf("isLoopbackBaseURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// The production constructor must install the SSRF guard for a non-loopback
// base URL, and must NOT install it for a loopback dev mock (so httptest /
// local relays keep working).
func TestNewAPIClient_GuardGatedByLoopback(t *testing.T) {
	t.Parallel()
	pub := newAPIClient("https://ilinkai.weixin.qq.com", "tok")
	if dc := dialContextOf(pub); dc == nil {
		t.Error("non-loopback client must have an SSRF DialContext installed")
	}

	loop := newAPIClient("http://127.0.0.1:9000", "tok")
	if dc := dialContextOf(loop); dc != nil {
		t.Error("loopback dev-mock client must NOT have a DialContext (default dialer)")
	}
}
