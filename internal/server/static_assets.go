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
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"strings"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

//go:embed static/manifest.json
var manifestJSON embed.FS

//go:embed static/sw.js
var swJS embed.FS

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
}

// staticAssets maps the asset key (basename used by handlers and the 304
// fast-path) to its cached bytes + ETag. Populated once at init.
var staticAssets = func() map[string]staticAsset {
	hash := func(b []byte) string {
		s := sha256.Sum256(b)
		return `"` + hex.EncodeToString(s[:16]) + `"`
	}
	read := func(fsys embed.FS, name string) (staticAsset, bool) {
		b, err := fsys.ReadFile(name)
		if err != nil {
			return staticAsset{}, false
		}
		return staticAsset{bytes: b, etag: hash(b)}, true
	}
	out := map[string]staticAsset{}
	for _, e := range []struct {
		key  string
		fsys embed.FS
		name string
	}{
		{"dashboard.html", dashboardHTML, "static/dashboard.html"},
		{"dashboard.js", dashboardJS, "static/dashboard.js"},
		{"agent_view.js", agentViewJS, "static/agent_view.js"},
		{"asset_browser.js", assetBrowserJS, "static/asset_browser.js"},
		{"manifest.json", manifestJSON, "static/manifest.json"},
		{"sw.js", swJS, "static/sw.js"},
	} {
		if a, ok := read(e.fsys, e.name); ok {
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
