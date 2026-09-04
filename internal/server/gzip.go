package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipPool reuses gzip.Writer instances across requests. BestSpeed gives ~3x
// on JSON with negligible CPU — the right tradeoff for a latency-sensitive UI.
var gzipPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// isCompressibleType reports whether a Content-Type value should be gzipped
// (explicit allowlist of text-shaped types; pre-compressed binaries gain nothing).
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

// acceptsGzip does a minimal alloc-free scan of an Accept-Encoding header for
// a "gzip" token that is not disabled by q=0. A missing header means
// "identity only" (RFC 7231 §5.3.4).
func acceptsGzip(ae string) bool {
	if ae == "" {
		return false
	}
	for ae != "" {
		var tok string
		if i := strings.IndexByte(ae, ','); i >= 0 {
			tok, ae = ae[:i], ae[i+1:]
		} else {
			tok, ae = ae, ""
		}
		tok = strings.TrimSpace(tok)
		name := tok
		params := ""
		if i := strings.IndexByte(tok, ';'); i >= 0 {
			name = strings.TrimSpace(tok[:i])
			params = tok[i+1:]
		}
		if !strings.EqualFold(name, "gzip") {
			continue
		}
		if !hasZeroQValue(params) {
			return true
		}
	}
	return false
}

// hasZeroQValue reports whether an Accept-Encoding parameter list contains
// a q-value that disables the token (q=0 or q=0.0...).
func hasZeroQValue(params string) bool {
	for params != "" {
		var p string
		if i := strings.IndexByte(params, ';'); i >= 0 {
			p, params = params[:i], params[i+1:]
		} else {
			p, params = params, ""
		}
		p = strings.TrimSpace(p)
		if len(p) < 2 || (p[0] != 'q' && p[0] != 'Q') || p[1] != '=' {
			continue
		}
		v := strings.TrimSpace(p[2:])
		if v == "0" || v == "0." || v == "0.0" || v == "0.00" || v == "0.000" {
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
	// Never double-encode a response that already has Content-Encoding.
	if h.Get("Content-Encoding") != "" {
		return
	}
	if !isCompressibleType(h.Get("Content-Type")) {
		return
	}
	g.useGzip = true
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(g.ResponseWriter)
	g.gz = gz
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	// Bodyless 304/204 must never be gzipped: close() would otherwise emit
	// ~20 bytes of gzip framing as a phantom body, which RFC 7232 forbids (#1771).
	if code == http.StatusNotModified || code == http.StatusNoContent {
		g.wroteHeader = true
		g.ResponseWriter.WriteHeader(code)
		return
	}
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

// Flush forwards to both the gzip writer and the underlying ResponseWriter so
// streaming handlers land bytes promptly instead of behind a gzip block boundary.
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
// advertises Accept-Encoding: gzip. Response-side only: it never touches
// r.Body, so inbound body caps stay with the request-parsing path.
func gzipMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrades must keep the raw ResponseWriter (Hijacker);
		// match on the Upgrade header so every WS route is covered.
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
