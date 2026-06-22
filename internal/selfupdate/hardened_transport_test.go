package selfupdate

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// TestHardenedTransport_PreservesDefaultTimeouts pins #2252: the SSRF-guarded
// transport must inherit http.DefaultTransport's TLSHandshakeTimeout,
// IdleConnTimeout, ExpectContinueTimeout and ForceAttemptHTTP2 rather than
// zero-valuing them. The pre-fix code built a bare
// &http.Transport{DialContext: dialCtx}, which left every default field at the
// zero value (no TLS-handshake deadline, no idle-conn reaping, HTTP/2 off).
func TestHardenedTransport_PreservesDefaultTimeouts(t *testing.T) {
	t.Parallel()

	def := http.DefaultTransport.(*http.Transport)
	dialCtx := func(context.Context, string, string) (net.Conn, error) { return nil, nil }
	got := hardenedTransport(dialCtx)

	if got.DialContext == nil {
		t.Fatal("hardenedTransport DialContext is nil — SSRF guard not wired")
	}
	if got.TLSHandshakeTimeout != def.TLSHandshakeTimeout || got.TLSHandshakeTimeout == 0 {
		t.Errorf("TLSHandshakeTimeout = %v, want default %v (non-zero)", got.TLSHandshakeTimeout, def.TLSHandshakeTimeout)
	}
	if got.IdleConnTimeout != def.IdleConnTimeout || got.IdleConnTimeout == 0 {
		t.Errorf("IdleConnTimeout = %v, want default %v (non-zero)", got.IdleConnTimeout, def.IdleConnTimeout)
	}
	if got.ExpectContinueTimeout != def.ExpectContinueTimeout || got.ExpectContinueTimeout == 0 {
		t.Errorf("ExpectContinueTimeout = %v, want default %v (non-zero)", got.ExpectContinueTimeout, def.ExpectContinueTimeout)
	}
	if got.ForceAttemptHTTP2 != def.ForceAttemptHTTP2 {
		t.Errorf("ForceAttemptHTTP2 = %v, want default %v", got.ForceAttemptHTTP2, def.ForceAttemptHTTP2)
	}
}
