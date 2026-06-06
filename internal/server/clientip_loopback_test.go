package server

import (
	"net/http"
	"testing"
)

// TestIsLoopbackClient pins R20260604-SEC-5: the pprof/expvar loopback gate
// must resolve the *real* client IP in trustedProxy mode rather than trusting
// r.RemoteAddr, which is always the proxy's loopback IP behind a reverse proxy.
func TestIsLoopbackClient(t *testing.T) {
	mk := func(remoteAddr, xff string) *http.Request {
		r := &http.Request{
			RemoteAddr: remoteAddr,
			Header:     http.Header{},
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	tests := []struct {
		name         string
		remoteAddr   string
		xff          string
		trustedProxy bool
		want         bool
	}{
		// Direct listener (no proxy): gate on RemoteAddr as before.
		{"direct loopback", "127.0.0.1:5050", "", false, true},
		{"direct ipv6 loopback", "[::1]:5050", "", false, true},
		{"direct external", "203.0.113.7:5050", "", false, false},
		{"direct uds empty", "", "", false, true},

		// THE BUG: behind a trusted proxy RemoteAddr is the proxy's loopback
		// IP for every forwarded request. Pre-fix isLoopbackRemote(RemoteAddr)
		// returned true here and waved the external caller through.
		{"proxied external is rejected", "127.0.0.1:5050", "203.0.113.9", true, false},
		{"proxied external chain rejected", "127.0.0.1:5050", "10.0.0.1, 203.0.113.9", true, false},

		// On-host SSH+curl 127.0.0.1 runbook: no proxy hop, no XFF, so
		// ClientIP collapses to the loopback RemoteAddr and stays allowed.
		{"on-host loopback no xff", "127.0.0.1:5050", "", true, true},

		// A proxied request whose real client genuinely is loopback (rare:
		// proxy and client co-located) stays allowed.
		{"proxied loopback client", "127.0.0.1:5050", "127.0.0.1", true, true},

		// Malformed XFF last hop → ClientIP falls back to RemoteAddr.
		{"proxied garbage xff falls back to loopback remote", "127.0.0.1:5050", "not-an-ip", true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isLoopbackClient(mk(tc.remoteAddr, tc.xff), tc.trustedProxy)
			if got != tc.want {
				t.Errorf("isLoopbackClient(remote=%q, xff=%q, trustedProxy=%v) = %v, want %v",
					tc.remoteAddr, tc.xff, tc.trustedProxy, got, tc.want)
			}
		})
	}
}

func TestIPStringIsLoopback(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.5", true},
		{"::1", true},
		{"", true},  // UDS RemoteAddr
		{"@", true}, // Linux abstract UDS
		{"203.0.113.1", false},
		{"10.0.0.1", false},
		{"garbage", false},
		{"127.0.0.1:5050", false}, // expects bare host, not host:port
	}
	for _, tc := range tests {
		if got := ipStringIsLoopback(tc.host); got != tc.want {
			t.Errorf("ipStringIsLoopback(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
