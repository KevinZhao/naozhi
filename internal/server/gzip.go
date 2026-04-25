package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipPool reuses gzip.Writer instances across requests so the dashboard's hot
// paths (events, sessions list) don't pay an allocation per response. Level 1
// (BestSpeed) gives ~3x compression on JSON payloads with negligible CPU cost
// versus default level 6 — the right tradeoff for a latency-sensitive UI on
// flaky networks.
var gzipPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// isCompressibleType reports whether a Content-Type value should be gzipped.
// Pre-compressed binary formats (image/*, audio/*, video/*, application/zip,
// application/gzip) gain nothing from a second pass and just burn CPU, so we
// keep an explicit allowlist of text-shaped types.
func isCompressibleType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	switch {
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/json", strings.HasSuffix(ct, "+json"):
		return true
	case ct == "application/javascript", ct == "application/ecmascript":
		return true
	case ct == "application/xml", strings.HasSuffix(ct, "+xml"):
		return true
	}
	return false
}

// acceptsGzip does a minimal scan of an Accept-Encoding header for the "gzip"
// token. Full q-value parsing is overkill here — every modern browser and
// reverse proxy sends gzip unconditionally, and a missing Accept-Encoding
// means "identity only" under RFC 7231 §5.3.4.
func acceptsGzip(ae string) bool {
	for _, tok := range strings.Split(ae, ",") {
		tok = strings.TrimSpace(tok)
		if i := strings.IndexByte(tok, ';'); i >= 0 {
			tok = strings.TrimSpace(tok[:i])
		}
		if strings.EqualFold(tok, "gzip") {
			return true
		}
	}
	return false
}

// gzipResponseWriter wraps an http.ResponseWriter and lazily switches to gzip
// encoding when the handler's Content-Type is a compressible text format.
// The decision is deferred until WriteHeader (or the first Write) because
// net/http handlers normally set Content-Type immediately before writing.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	useGzip     bool
}

func (g *gzipResponseWriter) decide() {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	// Never double-encode: if the handler already set Content-Encoding
	// (e.g. a pre-compressed blob), leave it alone.
	if h.Get("Content-Encoding") != "" {
		return
	}
	if !isCompressibleType(h.Get("Content-Type")) {
		return
	}
	g.useGzip = true
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	// Body length changes post-compression; any Content-Length the handler
	// computed against the uncompressed payload is now wrong.
	h.Del("Content-Length")
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(g.ResponseWriter)
	g.gz = gz
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(p))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.useGzip {
		return g.gz.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

// Flush forwards to both the gzip writer (so compressed bytes leave our buffer)
// and the underlying ResponseWriter. Currently no handler uses Flusher, but
// preserving the interface keeps the middleware transparent for future SSE /
// chunked-response code paths.
func (g *gzipResponseWriter) Flush() {
	if g.useGzip && g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// close flushes pending gzip bytes and returns the writer to the pool. Called
// by the middleware once the handler returns.
func (g *gzipResponseWriter) close() {
	if g.useGzip && g.gz != nil {
		_ = g.gz.Close()
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// gzipMiddleware wraps h with transparent gzip encoding when the client
// advertises Accept-Encoding: gzip. WebSocket upgrades are passed through
// verbatim so the underlying ResponseWriter keeps its Hijacker, and handlers
// that write pre-compressed binary (images, archives) are skipped via the
// Content-Type check in gzipResponseWriter.decide().
func gzipMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrades hijack the TCP connection — wrapping the
		// ResponseWriter would break the Hijacker assertion in gorilla/ws.
		// Matching on the Upgrade header is path-agnostic so future WS
		// routes (e.g. /ws-node) are covered automatically.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			h.ServeHTTP(w, r)
			return
		}
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		h.ServeHTTP(gw, r)
	})
}
