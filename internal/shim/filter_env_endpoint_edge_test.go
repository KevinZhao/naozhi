package shim

import "testing"

// TestValidateShimEndpointURL_EdgeLiterals extends the #1713 pin table with
// the literals a hand-rolled classifier tends to mishandle, now that range
// membership comes from envpolicy.ClassifyHost (#2300). Every row here was
// verified against the pre-#2300 shimEndpointInternalIP implementation; the
// table is the behaviour matrix, not a wish-list — do not flip a row without
// a policy decision.
func TestValidateShimEndpointURL_EdgeLiterals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		v       string
		wantErr bool
	}{
		// unspecified: rejected on https (shim is stricter than envpolicy here)
		{"https 0.0.0.0", "https://0.0.0.0/", true},
		{"https ::", "https://[::]/", true},
		{"http 0.0.0.0", "http://0.0.0.0/", true},

		// IPv4-mapped IPv6 classifies like the embedded IPv4
		{"https 4in6 private", "https://[::ffff:10.0.0.1]/", true},
		{"https 4in6 imds", "https://[::ffff:169.254.169.254]/", true},
		{"https 4in6 loopback", "https://[::ffff:127.0.0.1]:8443/", false},
		{"http 4in6 loopback", "http://[::ffff:127.0.0.1]:8080/", false},
		{"http 4in6 private", "http://[::ffff:10.0.0.1]/", true},
		{"https 4in6 public", "https://[::ffff:8.8.8.8]/", false},

		// link-local multicast and v6 link-local
		{"https ll multicast", "https://224.0.0.1/", true},
		{"https fe80", "https://[fe80::1]/", true},
		{"http fe80", "http://[fe80::1]/", true},

		// loopback range beyond the canonical literal
		{"https 127.0.0.2", "https://127.0.0.2/", false},
		{"http 127.255.255.254", "http://127.255.255.254:1/", false},

		// scheme case-insensitive, host case-insensitive
		{"HTTPS upper", "HTTPS://10.0.0.5/", true},
		{"HTTPS upper public", "HTTPS://Api.Anthropic.com/", false},
		{"HTTP upper localhost", "HTTP://LOCALHOST:8080/", false},
		{"HTTP upper remote", "HTTP://Example.com/", true},

		// port / userinfo / path / query must not smuggle a different host
		{"https private with port", "https://10.0.0.5:8443/v1", true},
		{"https userinfo before private", "https://api.anthropic.com@10.0.0.5/", true},
		{"https userinfo before imds", "https://user:pw@169.254.169.254/", true},
		{"https userinfo public", "https://user:pw@api.anthropic.com/", false},
		{"https imds in query only", "https://api.anthropic.com/?u=169.254.169.254", false},
		{"http loopback with userinfo", "http://u:p@127.0.0.1:9/", false},

		// boundary of RFC1918 172.16/12 and CGNAT
		{"https 172.32 public", "https://172.32.0.1/", false},
		{"https 172.31 private", "https://172.31.255.255/", true},
		{"https cgnat not private", "https://100.64.0.1/", false},

		// non-literal hosts fall through (hostname policy unchanged)
		{"https hostname", "https://gw.internal/", false},
		{"http hostname", "http://gw.internal/", true},

		// malformed
		{"empty", "", true},
		{"no scheme", "10.0.0.1", true},
		{"bad parse", "://bad", true},
		{"scheme-relative", "//10.0.0.1/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateShimEndpointURL(tc.v)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateShimEndpointURL(%q) err=%v, wantErr=%v", tc.v, err, tc.wantErr)
			}
		})
	}
}
