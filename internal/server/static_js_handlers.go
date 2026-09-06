package server

// static_js_handlers.go — one handler per extracted dashboard module (#2558
// D4). They are mechanical twins: 404 when the asset is missing, JS
// content-type + nosniff, revalidate, ETag short-circuit, body. Split out of
// static_assets.go so that file stays under the 500-line server-split limit as
// the module count grows.

import "net/http"

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

// handleAuthModalJS serves static/auth_modal.js (the token modal + session/node/profile pickers, imported by dashboard.js).
func handleAuthModalJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("auth_modal.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "auth_modal.js") {
		return
	}
	writeStaticAssetBody(w, r, "auth_modal.js")
}

// handleSendMessageJS serves static/send_message.js (the composer send path, imported by dashboard.js).
func handleSendMessageJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("send_message.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "send_message.js") {
		return
	}
	writeStaticAssetBody(w, r, "send_message.js")
}

// handleDashboardCSS serves static/css/*.css (the dashboard stylesheets split
// out of the inline <style> block, #2559). One handler for the directory: the
// file name comes from the request path and is looked up in the asset table,
// so an unknown name 404s instead of reaching the filesystem.
func handleDashboardCSS(w http.ResponseWriter, r *http.Request) {
	name := "css/" + r.PathValue("file")
	if staticAssetBytes(name) == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, name) {
		return
	}
	writeStaticAssetBody(w, r, name)
}
