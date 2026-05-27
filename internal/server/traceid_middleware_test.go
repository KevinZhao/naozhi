package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/ctxutil"
)

// TestWithTraceID_MintsWhenAbsent — the load-bearing case for #677:
// requests entering naozhi without an upstream X-Request-ID must still
// gain a trace id on the ctx and a matching response header so an
// operator can grep for the same value across HTTP and log pipelines.
func TestWithTraceID_MintsWhenAbsent(t *testing.T) {
	t.Parallel()

	var seen string
	h := withTraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxutil.TraceID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatalf("handler did not see a trace id on ctx")
	}
	if got := rec.Header().Get(traceIDHeader); got != seen {
		t.Fatalf("response header %s = %q; want %q (handler saw)", traceIDHeader, got, seen)
	}
	if len(seen) != 16 {
		t.Fatalf("minted trace id length = %d; want 16-hex-char id", len(seen))
	}
}

// TestWithTraceID_RespectsInbound — when an upstream proxy already
// stamped a request id (typical for ALB / sidecar / curl --header), we
// must propagate it unchanged rather than overwriting; otherwise
// distributed traces stitched at the proxy break at the naozhi hop.
func TestWithTraceID_RespectsInbound(t *testing.T) {
	t.Parallel()

	const upstream = "from-the-proxy-7f"
	var seen string
	h := withTraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxutil.TraceID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(traceIDHeader, upstream)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != upstream {
		t.Fatalf("ctx trace id = %q; want %q (inbound)", seen, upstream)
	}
	if got := rec.Header().Get(traceIDHeader); got != upstream {
		t.Fatalf("response header = %q; want %q (mirror)", got, upstream)
	}
}
