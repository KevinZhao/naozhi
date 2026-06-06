// Phase 5-prep / R-static-assets-extract (2026-05-28):
// 5 个 embed.FS 静态资源变量 + staticAssetETags init + serveStaticWithETag
// helper 抽到独立文件。纯物理切分、零行为变化。
//
// 这套基础设施被 4 个静态 handler（handleManifest / handleSW /
// handleDashboardJS / handleAgentViewJS，PR #1444 已转包级 func）共用，
// 加上 handleDashboard 也读 dashboardHTML 与调用 serveStaticWithETag。
// 单独成文件让 dashboard.go 聚焦 routing/registerDashboard 主流程。
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

//go:embed static/dashboard.js
var dashboardJS embed.FS

//go:embed static/agent_view.js
var agentViewJS embed.FS

//go:embed static/asset_browser.js
var assetBrowserJS embed.FS

// staticAsset is the once-read, immutable view of an embedded dashboard asset:
// its decompressed bytes and precomputed strong-form ETag. Go's
// embed.FS.ReadFile returns a *fresh copy* on every call (`[]byte(string)` —
// a heap alloc + memcpy of the whole file), so reading per request meant every
// dashboard.js (759 KB) / dashboard.html (207 KB) hit — including the ones that
// immediately return 304 — paid an allocation and copy of the entire file for
// no benefit. The bytes are baked in at build time and never change during a
// process's lifetime, so we read+hash each asset exactly once at init and have
// handlers reference the shared (read-only) slice. (#1771)
type staticAsset struct {
	bytes []byte
	etag  string
	// gz holds the gzip-compressed form of bytes, precomputed once at init for
	// compressible text assets. Because the content is immutable for the
	// process lifetime, we can afford max compression (gzip.BestCompression)
	// here — paid once, not per request. The shared gzipMiddleware uses
	// gzip.BestSpeed (level 1) because it compresses dynamic responses on the
	// fly; for these large, never-changing scripts level 9 cuts ~15% more
	// bytes off the wire (dashboard.js: level1≈297KB vs level9≈253KB) at zero
	// per-request CPU. nil when the asset is not precompressed (e.g. manifest
	// is tiny / sw is trivial). (#1769)
	gz []byte
}

// precompressGzip returns the gzip.BestCompression form of b, or nil if it did
// not actually shrink (tiny inputs can grow under gzip framing — never ship a
// "compressed" body bigger than the original). Used once at init for immutable
// embedded assets, so max compression is free; the result is copied out of the
// scratch buffer so we don't pin its (possibly larger) backing array.
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
		{"dashboard.js", dashboardJS, "static/dashboard.js", true},
		{"agent_view.js", agentViewJS, "static/agent_view.js", true},
		{"asset_browser.js", assetBrowserJS, "static/asset_browser.js", true},
		{"manifest.json", manifestJSON, "static/manifest.json", false},
		{"sw.js", swJS, "static/sw.js", false},
	} {
		if a, ok := read(e.fsys, e.name, e.compress); ok {
			out[e.key] = a
		}
	}
	return out
}()

// staticAssetETags preserves the legacy map[key]ETag shape for callers/tests
// that only need the ETag. Derived from staticAssets so there is a single
// source of truth. cron-dashboard-redesign P0 §6 — combined with the existing
// `Cache-Control: no-cache, must-revalidate`, ETag enables a 304 fast-path so
// browsers actually skip body bytes when content hasn't changed (no-cache
// alone forces every load to re-download, hurting both latency and bandwidth).
// Strong ETag form ("hex") is fine since byte equality is the actual semantics
// (no transformations).
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

// writeStaticAssetBody writes the asset body, preferring the precomputed
// gzip.BestCompression form when (a) it exists and (b) the client advertised
// gzip in Accept-Encoding. It sets Content-Encoding: gzip itself in that case,
// which gzipMiddleware.decide() honours (it never double-encodes a response
// that already has Content-Encoding set), so the middleware leaves the bytes
// untouched. Falls back to the raw bytes (middleware may still level-1 gzip
// those for non-precompressed assets). Caller must have already set
// Content-Type and any cache headers, and confirmed this is not a 304.
//
// Returns without writing when the client used If-None-Match and got a 304
// (caller handles that via serveStaticWithETag before calling this).
func writeStaticAssetBody(w http.ResponseWriter, r *http.Request, key string) {
	a := staticAssets[key]
	if a.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h := w.Header()
		h.Set("Content-Encoding", "gzip")
		// Vary so a shared cache doesn't hand a gzip body to an identity-only
		// client. Mirrors gzipMiddleware.decide().
		h.Add("Vary", "Accept-Encoding")
		// The compressed length differs from any Content-Length a caller may
		// have set against the raw bytes; drop it for defensive parity with
		// gzipMiddleware.decide() (no caller sets it today, but a future one
		// would otherwise ship a wrong length).
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
// It is intended to be called BEFORE the caller touches the (large) body bytes
// so a 304 costs no allocation. Other security headers (CSP/COOP/etc.) are
// still set by the caller before this point.
func serveStaticWithETag(w http.ResponseWriter, r *http.Request, assetKey string) bool {
	// Read the ETag straight from the cached-asset registry (single source of
	// truth) rather than the derived staticAssetETags copy, so the 304
	// fast-path does one map lookup, not two.
	tag := staticAssets[assetKey].etag
	if tag == "" {
		return false
	}
	w.Header().Set("ETag", tag)
	if match := r.Header.Get("If-None-Match"); match != "" {
		// Multiple If-None-Match values are comma-separated; do a simple
		// substring check rather than full RFC 7232 list parsing — our tag
		// is unique enough that a substring hit ≈ a real match. Edge case
		// (`*` wildcard) intentionally accepted: client wants any tag, so
		// 304 is correct.
		if match == "*" || strings.Contains(match, tag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}
