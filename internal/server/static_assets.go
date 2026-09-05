// 嵌入式 dashboard 静态资源：embed.FS 变量、一次性读取+ETag/gzip 预计算、
// serveStaticWithETag 304 fast-path。
package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

//go:embed static/manifest.json
var manifestJSON embed.FS

//go:embed static/sw.js
var swJS embed.FS

//go:embed static/nz_util.js
var nzUtilJS embed.FS

//go:embed static/contract.js
var contractJS embed.FS

//go:embed static/dashboard.js
var dashboardJS embed.FS

//go:embed static/cron_view.js
var cronViewJS embed.FS

//go:embed static/agent_view.js
var agentViewJS embed.FS

//go:embed static/asset_browser.js
var assetBrowserJS embed.FS

//go:embed static/files_view.js
var filesViewJS embed.FS

//go:embed static/favicon.svg
var faviconSVG embed.FS

// staticAsset is the once-read, immutable view of an embedded dashboard asset:
// its bytes and precomputed strong-form ETag. embed.FS.ReadFile copies the
// whole file on every call, so each asset is read+hashed exactly once at init
// and handlers share the read-only slice (#1771).
type staticAsset struct {
	bytes []byte
	etag  string
	// gz is the gzip.BestCompression form, precomputed once at init for
	// compressible assets (content is immutable, so level 9 is paid once and
	// beats the middleware's level 1 by ~15%). nil when not precompressed (#1769).
	gz []byte
}

// precompressGzip returns the gzip.BestCompression form of b, or nil if it did
// not actually shrink. The result is copied out of the scratch buffer so its
// (possibly larger) backing array is not pinned.
func precompressGzip(b []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(b); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(b) {
		return nil
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out
}

// staticAssets maps the asset key (basename used by handlers and the 304
// fast-path) to its cached bytes + ETag. Populated once at init.
var staticAssets = func() map[string]staticAsset {
	hash := func(b []byte) string {
		s := sha256.Sum256(b)
		return `"` + hex.EncodeToString(s[:16]) + `"`
	}
	read := func(fsys embed.FS, name string, compress bool) (staticAsset, bool) {
		b, err := fsys.ReadFile(name)
		if err != nil {
			return staticAsset{}, false
		}
		a := staticAsset{bytes: b, etag: hash(b)}
		if compress {
			a.gz = precompressGzip(b)
		}
		return a, true
	}
	out := map[string]staticAsset{}
	for _, e := range []struct {
		key      string
		fsys     embed.FS
		name     string
		compress bool
	}{
		{"dashboard.html", dashboardHTML, "static/dashboard.html", true},
		{"nz_util.js", nzUtilJS, "static/nz_util.js", true},
		{"contract.js", contractJS, "static/contract.js", true},
		{"dashboard.js", dashboardJS, "static/dashboard.js", true},
		{"cron_view.js", cronViewJS, "static/cron_view.js", true},
		{"agent_view.js", agentViewJS, "static/agent_view.js", true},
		{"asset_browser.js", assetBrowserJS, "static/asset_browser.js", true},
		{"files_view.js", filesViewJS, "static/files_view.js", true},
		{"manifest.json", manifestJSON, "static/manifest.json", false},
		{"sw.js", swJS, "static/sw.js", false},
		{"favicon.svg", faviconSVG, "static/favicon.svg", true},
	} {
		if a, ok := read(e.fsys, e.name, e.compress); ok {
			out[e.key] = a
		}
	}
	return out
}()

// staticAssetETags is the map[key]ETag view for callers/tests that only need
// the ETag; derived from staticAssets. Combined with `Cache-Control: no-cache,
// must-revalidate` the ETag enables the 304 fast-path.
var staticAssetETags = func() map[string]string {
	out := map[string]string{}
	for k, a := range staticAssets {
		out[k] = a.etag
	}
	return out
}()

// staticAssetBytes returns the cached, read-only bytes for an embedded asset.
// Callers MUST NOT mutate the returned slice — it is shared across all
// requests. Returns nil when the key is unknown (asset failed to embed).
func staticAssetBytes(key string) []byte {
	return staticAssets[key].bytes
}

// writeStaticAssetBody writes the asset body, preferring the precomputed gzip
// form when it exists and the client accepts gzip. Setting Content-Encoding
// here makes gzipMiddleware.decide() leave the bytes untouched. Caller must
// have set Content-Type / cache headers and handled the 304 case first.
func writeStaticAssetBody(w http.ResponseWriter, r *http.Request, key string) {
	a := staticAssets[key]
	if a.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h := w.Header()
		h.Set("Content-Encoding", "gzip")
		// Vary + drop Content-Length: mirrors gzipMiddleware.decide().
		h.Add("Vary", "Accept-Encoding")
		h.Del("Content-Length")
		if _, err := w.Write(a.gz); err != nil {
			slog.Debug("static asset gz write", "key", key, "err", err)
		}
		return
	}
	if _, err := w.Write(a.bytes); err != nil {
		slog.Debug("static asset write", "key", key, "err", err)
	}
}

// serveStaticWithETag attaches the asset's precomputed ETag and, on an
// If-None-Match hit, writes 304 and returns true so the caller skips the body.
// Call it BEFORE touching the body bytes; security headers (CSP/COOP/etc.)
// must already be set by the caller.
func serveStaticWithETag(w http.ResponseWriter, r *http.Request, assetKey string) bool {
	tag := staticAssets[assetKey].etag
	if tag == "" {
		return false
	}
	w.Header().Set("ETag", tag)
	if match := r.Header.Get("If-None-Match"); match != "" {
		// Substring check instead of full RFC 7232 list parsing: the tag is
		// unique enough that a substring hit is a real match.
		if match == "*" || strings.Contains(match, tag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}
