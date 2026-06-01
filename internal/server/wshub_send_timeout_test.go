package server

import (
	"testing"
	"time"
)

// TestRemoteNodeProxyTimeout locks the named timeout introduced by R244-ARCH-16
// (#1054). It previously lived as a bare `10*time.Second` literal duplicated at
// the proxied-interrupt and proxied-send sites; the constant is the single
// tunable source of truth, so a drift between the two sites (or an accidental
// retune) surfaces here.
func TestRemoteNodeProxyTimeout(t *testing.T) {
	if remoteNodeProxyTimeout != 10*time.Second {
		t.Fatalf("remoteNodeProxyTimeout = %v, want 10s", remoteNodeProxyTimeout)
	}
}
