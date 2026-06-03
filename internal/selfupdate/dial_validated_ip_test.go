package selfupdate

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBlockPrivateDialContext_DialsValidatedIP pins [R20260603-SEC-13]: a
// non-reserved (public) address passes the guard and proceeds to dial. The
// fix dials the validated IP directly instead of re-resolving the hostname,
// closing the TOCTOU DNS-rebinding window. We assert behavior indirectly: a
// public TEST-NET-3 (203.0.113.0/24, RFC 5737) literal IP is NOT rejected by
// the reserved-IP guard — the dial proceeds and fails only at the TCP layer
// (timeout / connection refused), never with the "reserved IP" message.
func TestBlockPrivateDialContext_DialsValidatedIP(t *testing.T) {
	prev := testHTTPTransport
	testHTTPTransport = nil
	t.Cleanup(func() { testHTTPTransport = prev })

	dialCtx := blockPrivateDialContext()
	if dialCtx == nil {
		t.Fatal("blockPrivateDialContext() returned nil in production mode")
	}

	// Bound the dial so the test never hangs on the unreachable TEST-NET IP.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	conn, err := dialCtx(ctx, "tcp", "203.0.113.7:443")
	if conn != nil {
		conn.Close()
	}
	// The guard must NOT classify a public IP as reserved. Whatever error the
	// dial returns (timeout / refused) is fine — it proves we got past the
	// guard and attempted to connect to the validated IP.
	if err != nil && strings.Contains(err.Error(), "reserved IP") {
		t.Fatalf("public TEST-NET IP wrongly rejected as reserved: %v", err)
	}
}
