package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestWithTraceID_RejectsUnsafeInbound — an oversized or control-byte-laden
// upstream id must not be reflected verbatim into the ctx / response header;
// the middleware mints a fresh 16-hex id instead so a malicious or
// misconfigured proxy cannot bloat correlated logs or smuggle control bytes.
func TestWithTraceID_RejectsUnsafeInbound(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"too_long":     strings.Repeat("a", maxInboundTraceIDLen+1),
		"crlf":         "abc\r\ninjected",
		"control_byte": "abc\x00def",
		"del":          "abc\x7fdef",
	}
	for name, inbound := range cases {
		inbound := inbound
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var seen string
			h := withTraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = ctxutil.TraceID(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(traceIDHeader, inbound)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if seen == inbound {
				t.Fatalf("unsafe inbound id was propagated verbatim: %q", inbound)
			}
			if len(seen) != 16 {
				t.Fatalf("expected a freshly minted 16-hex id, got %q (len %d)", seen, len(seen))
			}
			if got := rec.Header().Get(traceIDHeader); got != seen {
				t.Fatalf("response header %q != ctx id %q", got, seen)
			}
		})
	}
}

// TestAcceptInboundTraceID pins the accept/reject predicate directly.
func TestAcceptInboundTraceID(t *testing.T) {
	t.Parallel()
	accept := []string{"abc123", "550e8400-e29b-41d4-a716-446655440000", strings.Repeat("x", maxInboundTraceIDLen)}
	reject := []string{"", strings.Repeat("x", maxInboundTraceIDLen+1), "a\nb", "a\tb", "a\x00b"}
	for _, s := range accept {
		if !acceptInboundTraceID(s) {
			t.Errorf("acceptInboundTraceID(%q) = false; want true", s)
		}
	}
	for _, s := range reject {
		if acceptInboundTraceID(s) {
			t.Errorf("acceptInboundTraceID(%q) = true; want false", s)
		}
	}
}
