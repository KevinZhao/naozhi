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

// staticAssetETags holds precomputed strong-form ETags for embedded dashboard
// assets. cron-dashboard-redesign P0 §6 — combined with the existing
// `Cache-Control: no-cache, must-revalidate`, ETag enables a 304 fast-path so
// browsers actually skip body bytes when content hasn't changed (no-cache
// alone forces every load to re-download, hurting both latency and bandwidth).
//
// Because the asset bytes are baked in at build time via //go:embed, the
// SHA-256 hash never changes during a process's lifetime; computing it once at
// init is sufficient. Strong ETag form ("hex") is fine since byte equality is
// the actual semantics (no transformations).
var staticAssetETags = func() map[string]string {
	hash := func(b []byte) string {
		s := sha256.Sum256(b)
		return `"` + hex.EncodeToString(s[:16]) + `"`
	}
	out := map[string]string{}
	if b, err := dashboardHTML.ReadFile("static/dashboard.html"); err == nil {
		out["dashboard.html"] = hash(b)
	}
	if b, err := dashboardJS.ReadFile("static/dashboard.js"); err == nil {
		out["dashboard.js"] = hash(b)
	}
	if b, err := agentViewJS.ReadFile("static/agent_view.js"); err == nil {
		out["agent_view.js"] = hash(b)
	}
	if b, err := assetBrowserJS.ReadFile("static/asset_browser.js"); err == nil {
		out["asset_browser.js"] = hash(b)
	}
	return out
}()

// serveStaticWithETag writes the asset, attaching its precomputed ETag and
// honouring If-None-Match for a 304 fast-path. Returns true when a 304 was
// served so the caller can skip writing the body. Other security headers
// (CSP/COOP/etc.) are still set by the caller before this point.
func serveStaticWithETag(w http.ResponseWriter, r *http.Request, assetKey string) bool {
	tag := staticAssetETags[assetKey]
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
