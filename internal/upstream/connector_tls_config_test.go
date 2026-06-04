package upstream

import (
	"crypto/tls"
	"testing"
)

// TestConnectorTLSConfig pins R20260603150052-GO-3 (#1711): the TLS floor is
// always pinned at 1.2, and the upstream.insecure flag is actually consumed —
// InsecureSkipVerify must track the flag rather than being silently ignored.
func TestConnectorTLSConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		insecure bool
	}{
		{"secure", false},
		{"insecure", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := connectorTLSConfig(tc.insecure)
			if cfg == nil {
				t.Fatal("connectorTLSConfig returned nil")
			}
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %x, want TLS1.2 %x", cfg.MinVersion, tls.VersionTLS12)
			}
			if cfg.InsecureSkipVerify != tc.insecure {
				t.Errorf("InsecureSkipVerify = %v, want %v (flag must be consumed)", cfg.InsecureSkipVerify, tc.insecure)
			}
		})
	}
}
