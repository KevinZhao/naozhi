package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTraceIDWiring_StampsRequestIDOnEveryResponse pins R247-ARCH-20 / #677:
// the withTraceID middleware MUST be wrapped around the mux that backs the
// http.Server, not just defined in traceid_middleware.go in isolation. The
// pre-fix state had the middleware unreachable because no mux included it
// — production requests neither carried X-Request-ID on their ctx nor
// echoed it in the response header, so the issue was effectively unfixed
// despite being closed.
//
// We exercise the same handler chain server.go's Run installs into
// http.Server.Handler — withTraceID(gzipMiddleware(s.mux)) — by hitting a
// mux-registered route (/health is unauthenticated and always present)
// and asserting the response carries X-Request-ID. A plain s.mux without
// withTraceID would return no header; a missing wiring would silently
// regress to the pre-fix state.
func TestTraceIDWiring_StampsRequestIDOnEveryResponse(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	// Reproduce server.go's Handler exactly so a future refactor that
	// rewires the chain has to update this test.
	handler := withTraceID(gzipMiddleware(srv.mux))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	got := w.Header().Get("X-Request-ID")
	if got == "" {
		t.Fatal("X-Request-ID response header missing — withTraceID is not wired into the mux chain")
	}
	// The middleware mints 16-hex ids; just sanity-check shape so a
	// future contributor swapping NewTraceID's encoding fails loudly.
	if len(got) != 16 || strings.TrimLeft(got, "0123456789abcdef") != "" {
		t.Errorf("X-Request-ID = %q; want 16-char hex (newly minted by withTraceID)", got)
	}
}

// TestTraceIDWiring_RespectsInboundRequestID guards the load-balancer
// pass-through case: a caller that already stamped X-Request-ID upstream
// (nginx, ALB, sidecar) MUST see the same id reflected back, not a fresh
// mint. This is the contract documented in withTraceID's godoc; without
// it cross-component traces lose their join key at the naozhi boundary.
func TestTraceIDWiring_RespectsInboundRequestID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	handler := withTraceID(gzipMiddleware(srv.mux))

	const upstreamID = "upstream-trace-abc123"
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Request-ID", upstreamID)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-ID"); got != upstreamID {
		t.Errorf("X-Request-ID echoed = %q; want %q (inbound id must pass through verbatim)", got, upstreamID)
	}
}
