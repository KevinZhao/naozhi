package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// TestReverseNodeTLSGate enforces the #1026 (R226-SEC-3) precondition: the
// /ws-node upgrade must fail closed over plaintext, non-loopback transports so
// the node token in the first message is never sent in cleartext. The four axes
// are TLS / X-Forwarded-Proto / loopback-remote / trusted_proxy. A regression
// that lets a public plaintext upgrade through (e.g. inverted condition) would
// re-open the passive-sniff window the gate exists to close.
func TestReverseNodeTLSGate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		trustedProxy bool
		tls          bool
		xfProto      string
		remoteAddr   string
		wantNext     bool // true => upgrade allowed (gate passes through)
	}{
		// Plaintext, public remote, no proxy: reject.
		{"plaintext_public_noproxy", false, false, "", "203.0.113.5:5555", false},
		// Plaintext, public remote, proxy but not https: reject.
		{"plaintext_public_proxy_http", true, false, "http", "203.0.113.5:5555", false},
		// Trusted proxy terminated TLS upstream: allow.
		{"proxied_https", true, false, "https", "203.0.113.5:5555", true},
		// Direct TLS: allow.
		{"direct_tls", false, true, "", "203.0.113.5:5555", true},
		// Loopback plaintext (token never hits the wire): allow.
		{"loopback_v4", false, false, "", "127.0.0.1:5555", true},
		{"loopback_v6", false, false, "", "[::1]:5555", true},
		// UDS (empty RemoteAddr) is treated as loopback: allow.
		{"uds_empty_remote", false, false, "", "", true},
		// X-Forwarded-Proto: https but trusted_proxy off — header is untrusted, reject.
		{"untrusted_xfproto_https", false, false, "https", "203.0.113.5:5555", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{auth: auth.New("", []byte("test-secret-1234"), "g0", tc.trustedProxy)}

			var called bool
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
			gate := s.reverseNodeTLSGate(next)

			req := httptest.NewRequest(http.MethodGet, "/ws-node", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xfProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.xfProto)
			}
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, req)

			if called != tc.wantNext {
				t.Fatalf("next called = %v, want %v (status=%d)", called, tc.wantNext, rec.Code)
			}
			if !tc.wantNext && rec.Code != http.StatusUpgradeRequired {
				t.Errorf("rejected upgrade status = %d, want %d", rec.Code, http.StatusUpgradeRequired)
			}
		})
	}
}
