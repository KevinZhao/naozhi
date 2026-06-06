package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStaticAssetBytes_Cached verifies the embedded asset bytes are read once
// at init and shared, not re-copied per request. Before #1771 each handler
// called embed.FS.ReadFile (a fresh heap copy of the whole file) on every
// request — including ones that immediately 304. We assert the same backing
// array is returned across calls (identity), which only holds for a cached
// slice, never for embed.FS.ReadFile's per-call copy.
func TestStaticAssetBytes_Cached(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"dashboard.html", "dashboard.js", "agent_view.js", "asset_browser.js", "manifest.json", "sw.js"} {
		a := staticAssetBytes(key)
		if a == nil {
			t.Fatalf("staticAssetBytes(%q) = nil; asset failed to embed", key)
		}
		b := staticAssetBytes(key)
		// Identity check: cached slices share the same backing array, so the
		// pointer to the first element is stable. embed.FS.ReadFile would
		// return a fresh allocation each call (different pointer).
		if &a[0] != &b[0] {
			t.Errorf("staticAssetBytes(%q) returned a fresh copy across calls; expected a shared cached slice", key)
		}
	}
}

// TestStaticAssetETags_DerivedFromCache verifies the legacy ETag map stays in
// sync with the cached-asset registry (single source of truth).
func TestStaticAssetETags_DerivedFromCache(t *testing.T) {
	t.Parallel()
	for k, a := range staticAssets {
		if staticAssetETags[k] != a.etag {
			t.Errorf("staticAssetETags[%q]=%q out of sync with staticAssets etag %q", k, staticAssetETags[k], a.etag)
		}
	}
}

// TestServeStaticWithETag_304BeforeBody verifies the 304 fast-path sets the
// ETag and returns true (caller skips body) on an If-None-Match hit, and
// returns false otherwise. This is the contract that lets handlers reference
// cached bytes without allocating on the 304 path.
func TestServeStaticWithETag_304BeforeBody(t *testing.T) {
	t.Parallel()
	tag := staticAssetETags["dashboard.js"]
	if tag == "" {
		t.Fatal("missing ETag for dashboard.js")
	}

	// Hit: If-None-Match matches → 304, returns true.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	r.Header.Set("If-None-Match", tag)
	if got := serveStaticWithETag(w, r, "dashboard.js"); !got {
		t.Errorf("serveStaticWithETag on If-None-Match hit = false, want true")
	}
	if w.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", w.Code)
	}
	if w.Header().Get("ETag") != tag {
		t.Errorf("ETag header = %q, want %q", w.Header().Get("ETag"), tag)
	}

	// Miss: no If-None-Match → returns false (caller writes body), ETag set.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	if got := serveStaticWithETag(w2, r2, "dashboard.js"); got {
		t.Errorf("serveStaticWithETag without If-None-Match = true, want false")
	}
	if w2.Header().Get("ETag") != tag {
		t.Errorf("ETag header on miss = %q, want %q", w2.Header().Get("ETag"), tag)
	}
}

// TestManifestAndSW_304FastPath pins #1771: manifest.json and sw.js are now in
// the cached registry and serve an ETag, so a conditional re-request (sw.js is
// no-cache, so browsers re-check it on every SW update) returns 304 with an
// empty body instead of re-downloading.
func TestManifestAndSW_304FastPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key     string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"manifest.json", "/manifest.json", handleManifest},
		{"sw.js", "/sw.js", handleSW},
	}
	for _, tc := range cases {
		tag := staticAssets[tc.key].etag
		if tag == "" {
			t.Fatalf("%s missing cached ETag", tc.key)
		}
		// Conditional GET with matching ETag → 304, empty body.
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("If-None-Match", tag)
		tc.handler(w, r)
		if w.Code != http.StatusNotModified {
			t.Errorf("%s conditional GET status = %d, want 304", tc.key, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("%s 304 carries %d-byte body, want empty", tc.key, w.Body.Len())
		}
		if w.Header().Get("ETag") != tag {
			t.Errorf("%s 304 ETag = %q, want %q", tc.key, w.Header().Get("ETag"), tag)
		}
		// Unconditional GET → 200 with body + ETag.
		w2 := httptest.NewRecorder()
		r2 := httptest.NewRequest(http.MethodGet, tc.path, nil)
		tc.handler(w2, r2)
		if w2.Code != http.StatusOK {
			t.Errorf("%s GET status = %d, want 200", tc.key, w2.Code)
		}
		if w2.Body.Len() == 0 {
			t.Errorf("%s GET returned empty body", tc.key)
		}
		if w2.Header().Get("ETag") != tag {
			t.Errorf("%s GET ETag = %q, want %q", tc.key, w2.Header().Get("ETag"), tag)
		}
	}
}

// TestGzipMiddleware_Skips304 pins #1771: a 304 response must NOT be gzip
// encoded. Before the fix, gzipResponseWriter.WriteHeader(304) ran decide(),
// which (for a compressible Content-Type) set Content-Encoding: gzip, pulled a
// gzip.Writer from the pool, and on close() wrote a ~20-byte gzip frame as a
// phantom body — a 304 must carry no body (RFC 7232).
func TestGzipMiddleware_Skips304(t *testing.T) {
	t.Parallel()
	// Handler mimics a static JS handler that sets a compressible Content-Type
	// then 304s via the ETag fast-path.
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.WriteHeader(http.StatusNotModified)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("304 response has Content-Encoding=%q, want none", enc)
	}
	if body := w.Body.Bytes(); len(body) != 0 {
		t.Errorf("304 response carries a %d-byte body (phantom gzip frame?), want empty", len(body))
	}
}

// TestGzipMiddleware_Skips204 mirrors the 304 case for 204 No Content.
func TestGzipMiddleware_Skips204(t *testing.T) {
	t.Parallel()
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("204 response has Content-Encoding=%q, want none", enc)
	}
	if body := w.Body.Bytes(); len(body) != 0 {
		t.Errorf("204 response carries a %d-byte body, want empty", len(body))
	}
}

// TestGzipMiddleware_Still200Compresses guards against over-broad short-circuit:
// a normal 200 with compressible content must still be gzipped.
func TestGzipMiddleware_Still200Compresses(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("dashboard bytes ", 512) // compressible
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(payload))
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("200 compressible response Content-Encoding=%q, want gzip", enc)
	}
	if w.Body.Len() >= len(payload) {
		t.Errorf("200 body not compressed: got %d bytes, raw was %d", w.Body.Len(), len(payload))
	}
}
