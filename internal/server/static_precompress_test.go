package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardHTML_ScriptsDeferred pins #1769: the three external dashboard
// scripts must carry `defer` so the parser isn't blocked downloading/parsing
// 759KB+ of JS. defer preserves execution order (dashboard.js exports symbols
// the other two consume) and runs before DOMContentLoaded.
func TestDashboardHTML_ScriptsDeferred(t *testing.T) {
	t.Parallel()
	data := staticAssetBytes("dashboard.html")
	if data == nil {
		t.Fatal("dashboard.html not embedded")
	}
	html := string(data)
	for _, src := range []string{"/static/dashboard.js", "/static/agent_view.js", "/static/asset_browser.js"} {
		want := `<script defer src="` + src + `">`
		if !strings.Contains(html, want) {
			t.Errorf("dashboard.html: %q must be loaded with defer; missing %q", src, want)
		}
		// Guard against a non-deferred tag regressing back in.
		bad := `<script src="` + src + `">`
		if strings.Contains(html, bad) {
			t.Errorf("dashboard.html: %q is loaded WITHOUT defer (%q) — blocks the parser", src, bad)
		}
	}
}

// TestPrecompressGzip_Guards covers the helper's edge cases: a tiny input that
// would grow under gzip framing returns nil (never ship a larger "compressed"
// body), and a compressible input returns valid gzip that decodes to the
// original.
func TestPrecompressGzip_Guards(t *testing.T) {
	t.Parallel()
	// Tiny / incompressible input → nil (gzip framing overhead exceeds savings).
	if got := precompressGzip([]byte("x")); got != nil {
		t.Errorf("precompressGzip(tiny) = %d bytes, want nil (no shrink)", len(got))
	}
	if got := precompressGzip(nil); got != nil {
		t.Errorf("precompressGzip(nil) = %d bytes, want nil", len(got))
	}
	// Compressible input → valid gzip, smaller, decodes back.
	raw := []byte(strings.Repeat("compress me ", 1000))
	gz := precompressGzip(raw)
	if gz == nil {
		t.Fatal("precompressGzip(compressible) = nil, want compressed bytes")
	}
	if len(gz) >= len(raw) {
		t.Errorf("precompressGzip did not shrink: %d >= %d", len(gz), len(raw))
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, raw) {
		t.Error("precompressGzip output does not decode to the original")
	}
}

// TestStaticAsset_Precompressed verifies #1769: compressible static assets are
// gzip.BestCompression-precompressed once at init, and the precompressed bytes
// are served (with Content-Encoding: gzip) to gzip-capable clients — never
// re-compressed on the fly by gzipMiddleware.
func TestStaticAsset_Precompressed(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"dashboard.html", "dashboard.js", "agent_view.js", "asset_browser.js"} {
		a := staticAssets[key]
		if a.gz == nil {
			t.Errorf("%s has no precompressed form; expected gzip.BestCompression cache", key)
			continue
		}
		// Precompressed must be smaller than raw.
		if len(a.gz) >= len(a.bytes) {
			t.Errorf("%s gz (%d) not smaller than raw (%d)", key, len(a.gz), len(a.bytes))
		}
		// Precompressed bytes must decode back to the exact raw bytes.
		zr, err := gzip.NewReader(bytes.NewReader(a.gz))
		if err != nil {
			t.Errorf("%s gz not a valid gzip stream: %v", key, err)
			continue
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Errorf("%s gz decode error: %v", key, err)
			continue
		}
		if !bytes.Equal(got, a.bytes) {
			t.Errorf("%s gz decodes to %d bytes, want %d (raw mismatch)", key, len(got), len(a.bytes))
		}
	}
}

// TestStaticAsset_PrecompressedBeatsLevel1 confirms the level-9 init-time
// compression is meaningfully smaller than the level-1 on-the-fly path the
// middleware would otherwise use — the whole point of precompressing.
func TestStaticAsset_PrecompressedBeatsLevel1(t *testing.T) {
	t.Parallel()
	a := staticAssets["dashboard.js"]
	if a.gz == nil {
		t.Skip("dashboard.js not embedded")
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write(a.bytes)
	_ = zw.Close()
	level1 := buf.Len()
	if a.gz == nil || len(a.gz) >= level1 {
		t.Errorf("precompressed dashboard.js (%d) should beat level-1 (%d)", len(a.gz), level1)
	}
	t.Logf("dashboard.js: raw=%d level1=%d level9=%d (%.1f%% smaller than level1)",
		len(a.bytes), level1, len(a.gz), 100*float64(level1-len(a.gz))/float64(level1))
}

// TestHandleDashboardJS_ServesPrecompressedThroughMiddleware drives the real
// handler through gzipMiddleware and asserts: a gzip client gets the cached
// level-9 body (Content-Encoding: gzip, decodes to raw, NOT re-compressed by
// the middleware), and an identity client gets raw bytes.
func TestHandleDashboardJS_ServesPrecompressedThroughMiddleware(t *testing.T) {
	t.Parallel()
	h := gzipMiddleware(http.HandlerFunc(handleDashboardJS))
	a := staticAssets["dashboard.js"]

	// gzip-capable client → precompressed level-9 body.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", w.Header().Get("Content-Encoding"))
	}
	body := w.Body.Bytes()
	// Body must equal the cached precompressed bytes (not a fresh level-1
	// re-compression by the middleware).
	if !bytes.Equal(body, a.gz) {
		t.Errorf("served body (%d bytes) != cached precompressed bytes (%d)", len(body), len(a.gz))
	}
	// And it must decode to the raw script.
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("served body not valid gzip: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, a.bytes) {
		t.Errorf("served gzip decodes to %d bytes, want raw %d", len(got), len(a.bytes))
	}
	if vary := w.Header().Get("Vary"); vary == "" {
		t.Errorf("missing Vary: Accept-Encoding on gzip response")
	}

	// identity client → raw bytes, no Content-Encoding.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/static/dashboard.js", nil)
	// no Accept-Encoding → middleware bypasses, handler writes raw
	h.ServeHTTP(w2, r2)
	if w2.Header().Get("Content-Encoding") != "" {
		t.Errorf("identity client got Content-Encoding=%q, want none", w2.Header().Get("Content-Encoding"))
	}
	if !bytes.Equal(w2.Body.Bytes(), a.bytes) {
		t.Errorf("identity body (%d) != raw bytes (%d)", w2.Body.Len(), len(a.bytes))
	}
}
