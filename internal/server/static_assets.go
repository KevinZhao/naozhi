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

//go:embed static/render_md.js
var renderMdJS embed.FS

//go:embed static/self_update.js
var selfUpdateJS embed.FS

//go:embed static/voice.js
var voiceJS embed.FS

//go:embed static/session_header.js
var sessionHeaderJS embed.FS

//go:embed static/composer_files.js
var composerFilesJS embed.FS

//go:embed static/mobile_nav.js
var mobileNavJS embed.FS

//go:embed static/split_view.js
var splitViewJS embed.FS

//go:embed static/system_view.js
var systemViewJS embed.FS

//go:embed static/running_banner.js
var runningBannerJS embed.FS

//go:embed static/file_refs.js
var fileRefsJS embed.FS

//go:embed static/utilities.js
var utilitiesJS embed.FS

//go:embed static/discovery.js
var discoveryJS embed.FS

//go:embed static/tuning.js
var tuningJS embed.FS

//go:embed static/msg_nav.js
var msgNavJS embed.FS

//go:embed static/sidebar_project.js
var sidebarProjectJS embed.FS

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
		{"render_md.js", renderMdJS, "static/render_md.js", true},
		{"self_update.js", selfUpdateJS, "static/self_update.js", true},
		{"voice.js", voiceJS, "static/voice.js", true},
		{"session_header.js", sessionHeaderJS, "static/session_header.js", true},
		{"composer_files.js", composerFilesJS, "static/composer_files.js", true},
		{"mobile_nav.js", mobileNavJS, "static/mobile_nav.js", true},
		{"split_view.js", splitViewJS, "static/split_view.js", true},
		{"system_view.js", systemViewJS, "static/system_view.js", true},
		{"running_banner.js", runningBannerJS, "static/running_banner.js", true},
		{"file_refs.js", fileRefsJS, "static/file_refs.js", true},
		{"utilities.js", utilitiesJS, "static/utilities.js", true},
		{"discovery.js", discoveryJS, "static/discovery.js", true},
		{"tuning.js", tuningJS, "static/tuning.js", true},
		{"msg_nav.js", msgNavJS, "static/msg_nav.js", true},
		{"sidebar_project.js", sidebarProjectJS, "static/sidebar_project.js", true},
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

// handleRenderMdJS serves static/render_md.js (markdown / KaTeX / mermaid
// rendering, imported by dashboard.js).
func handleRenderMdJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("render_md.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "render_md.js") {
		return
	}
	writeStaticAssetBody(w, r, "render_md.js")
}

// handleSelfUpdateJS serves static/self_update.js (self-update chip,
// imported by dashboard.js).
func handleSelfUpdateJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("self_update.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "self_update.js") {
		return
	}
	writeStaticAssetBody(w, r, "self_update.js")
}

// handleVoiceJS serves static/voice.js (hold-to-talk voice input, imported
// by dashboard.js).
func handleVoiceJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("voice.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "voice.js") {
		return
	}
	writeStaticAssetBody(w, r, "voice.js")
}

// handleSessionHeaderJS serves static/session_header.js (the chat header run-history panel + status chips, imported by dashboard.js).
func handleSessionHeaderJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("session_header.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "session_header.js") {
		return
	}
	writeStaticAssetBody(w, r, "session_header.js")
}

// handleComposerFilesJS serves static/composer_files.js (composer attachments, imported by dashboard.js).
func handleComposerFilesJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("composer_files.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "composer_files.js") {
		return
	}
	writeStaticAssetBody(w, r, "composer_files.js")
}

// handleMobileNavJS serves static/mobile_nav.js (mobile shell navigation, imported by dashboard.js).
func handleMobileNavJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("mobile_nav.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "mobile_nav.js") {
		return
	}
	writeStaticAssetBody(w, r, "mobile_nav.js")
}

// handleSplitViewJS serves static/split_view.js (desktop split-view docking, imported by dashboard.js).
func handleSplitViewJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("split_view.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "split_view.js") {
		return
	}
	writeStaticAssetBody(w, r, "split_view.js")
}

// handleSystemViewJS serves static/system_view.js (the 系统 top-level view, imported by dashboard.js).
func handleSystemViewJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("system_view.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "system_view.js") {
		return
	}
	writeStaticAssetBody(w, r, "system_view.js")
}

// handleRunningBannerJS serves static/running_banner.js (the transcript running banner, imported by dashboard.js).
func handleRunningBannerJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("running_banner.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "running_banner.js") {
		return
	}
	writeStaticAssetBody(w, r, "running_banner.js")
}

// handleFileRefsJS serves static/file_refs.js (file-reference buttons + preview drawer, imported by dashboard.js).
func handleFileRefsJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("file_refs.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "file_refs.js") {
		return
	}
	writeStaticAssetBody(w, r, "file_refs.js")
}

// handleUtilitiesJS serves static/utilities.js (shared dashboard utilities:
// dialogs, time/cost formatting, toasts, clipboard helpers).
func handleUtilitiesJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("utilities.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "utilities.js") {
		return
	}
	writeStaticAssetBody(w, r, "utilities.js")
}

// handleDiscoveryJS serves static/discovery.js (discovered-session preview + takeover, imported by dashboard.js).
func handleDiscoveryJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("discovery.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "discovery.js") {
		return
	}
	writeStaticAssetBody(w, r, "discovery.js")
}

// handleTuningJS serves static/tuning.js (the per-session model/effort tuning popover, imported by dashboard.js).
func handleTuningJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("tuning.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "tuning.js") {
		return
	}
	writeStaticAssetBody(w, r, "tuning.js")
}

// handleMsgNavJS serves static/msg_nav.js (transcript message navigation, imported by dashboard.js).
func handleMsgNavJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("msg_nav.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "msg_nav.js") {
		return
	}
	writeStaticAssetBody(w, r, "msg_nav.js")
}

// handleSidebarProjectJS serves static/sidebar_project.js (sidebar project headers + project settings, imported by dashboard.js).
func handleSidebarProjectJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("sidebar_project.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "sidebar_project.js") {
		return
	}
	writeStaticAssetBody(w, r, "sidebar_project.js")
}
