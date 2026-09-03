package envpolicy

import "testing"

// TestValidateBaseURLValue_EdgeLiterals pins ValidateBaseURLValue on the
// literals the shared classifier (#2300) is most likely to be blamed for:
// IPv4-mapped IPv6, the unspecified address, link-local multicast, scheme /
// host casing, userinfo, and the RFC1918 boundary. Every row reflects the
// pre-#2300 behaviour of this function — including the rows marked
// "owner decision", which document a known gap rather than endorse it.
// The hatch env var is pinned closed for the whole test.
func TestValidateBaseURLValue_EdgeLiterals(t *testing.T) {
	t.Setenv("NAOZHI_ALLOW_PRIVATE_BASE_URL", "")
	cases := []struct {
		name    string
		v       string
		wantErr bool
	}{
		// IPv4-mapped IPv6 classifies like the embedded IPv4
		{"https 4in6 private", "https://[::ffff:10.0.0.1]/", true},
		{"https 4in6 imds", "https://[::ffff:169.254.169.254]/", true},
		{"https 4in6 loopback", "https://[::ffff:127.0.0.1]/", true}, // loopback https rejected by default (#2278)
		{"http 4in6 loopback", "http://[::ffff:127.0.0.1]:8080/", false},
		{"http 4in6 private", "http://[::ffff:10.0.0.1]/", true},
		{"https 4in6 public", "https://[::ffff:8.8.8.8]/", false},

		// unspecified
		{"http 0.0.0.0", "http://0.0.0.0/", true},
		{"http ::", "http://[::]/", true},
		// Owner decision (#2300 follow-up): https to the unspecified address is
		// currently ALLOWED here, whereas the shim endpoint guard rejects it.
		// Pinned as-is so a refactor cannot silently change it either way.
		{"https 0.0.0.0 (owner decision: currently allowed)", "https://0.0.0.0/", false},
		{"https :: (owner decision: currently allowed)", "https://[::]/", false},

		// link-local multicast / v6 link-local
		{"https ll multicast", "https://224.0.0.1/", true},
		{"http ll multicast", "http://224.0.0.1/", true},
		{"https fe80", "https://[fe80::1]/", true},

		// loopback range beyond the canonical literal
		{"http 127.0.0.2", "http://127.0.0.2/", false},
		{"http 127.255.255.254", "http://127.255.255.254:1/", false},

		// casing
		{"HTTPS upper private", "HTTPS://10.0.0.5/", true},
		{"HTTPS upper public", "HTTPS://Api.Anthropic.com/", false},
		{"HTTP upper localhost", "HTTP://LOCALHOST:8080/", false},
		{"HTTP upper remote", "HTTP://Example.com/", true},

		// port / userinfo / query must not smuggle a different host
		{"https private with port", "https://10.0.0.5:8443/v1", true},
		{"https userinfo before private", "https://api.anthropic.com@10.0.0.5/", true},
		{"https userinfo before imds", "https://user:pw@169.254.169.254/", true},
		{"https userinfo public", "https://user:pw@api.anthropic.com/", false},
		{"https imds in query only", "https://api.anthropic.com/?u=169.254.169.254", false},

		// RFC1918 boundary / CGNAT
		{"https 172.32 public", "https://172.32.0.1/", false},
		{"https 172.31 private", "https://172.31.255.255/", true},
		{"https cgnat not private", "https://100.64.0.1/", false},

		// non-literal hosts fall through on https, rejected on http
		{"https hostname", "https://gw.internal/", false},
		{"http hostname", "http://gw.internal/", true},

		// malformed / other schemes
		{"no scheme", "10.0.0.1", true},
		{"scheme-relative", "//10.0.0.1/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBaseURLValue(tc.v)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateBaseURLValue(%q) err=%v, wantErr=%v", tc.v, err, tc.wantErr)
			}
		})
	}
}
