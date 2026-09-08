package project

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// serveRender streams a workspace .html / .svg so the dashboard can embed it
// as a blob URL inside a sandboxed iframe (coverage reports, Playwright trace,
// generated SVG diagrams). Workspace files are untrusted (the CLI can write
// <script> into any of them) and rendering them same-origin is stored XSS.
// Firefox ignores the `CSP: sandbox` header on top-level navigation and
// X-Frame-Options only covers embedding, so this handler deliberately serves
// application/octet-stream + attachment: a direct navigation downloads, while
// the dashboard fetch() wraps the bytes in a Blob (opaque origin) for the
// iframe. MIME gating stays server-side; size cap mirrors serveRaw. Relative
// resources inside the document do not resolve (blob URL has no base path).
func (h *Handlers) serveRender(w http.ResponseWriter, r *http.Request, f *os.File, rootResolved, resolved string, info os.FileInfo) {
	// Mirror servePreview / serveRaw / serveDownload: refuse credential-bearing
	// names (full-path scan) even when the bytes sniff as HTML/SVG, otherwise
	// a renamed .env with HTML-shaped contents is readable via render mode.
	if isSensitiveDownloadPath(workspaceScanPath(rootResolved, resolved)) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "render blocked for sensitive file name"})
		return
	}
	if info.Size() > maxRawBytes {
		httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline render; use download mode"})
		return
	}

	// fd opened once by HandleFileGet (O_NOFOLLOW); caller owns Close.

	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	mime := detectMime(resolved, head[:n])

	// Normalize to base MIME (strip charset params) before whitelist check.
	// detectMime returns "text/html; charset=utf-8" for real HTML payloads,
	// which must still match the "text/html" gate.
	base := mime
	if i := strings.Index(mime, ";"); i > 0 {
		base = strings.TrimSpace(mime[:i])
	}
	// Strict whitelist — only HTML/XHTML and SVG flow through render. PDF,
	// raster images, and text route through their dedicated handlers (preview/
	// raw/download). detectMime pins .svg to image/svg+xml regardless of byte
	// sniff, so an attacker cannot reach this branch with non-SVG bytes by
	// renaming a .html file to .svg — the extension is authoritative for SVG.
	if base != "text/html" && base != "application/xhtml+xml" && base != "image/svg+xml" {
		httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "render mode supports HTML and SVG only; use preview/raw/download for other types"})
		return
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}

	// inline=1 (#1980): the dashboard preview iframe points its src directly
	// here — an iframe document's CSP comes from THIS response, not the
	// parent page, which is what keeps workspace HTML rendering after the
	// dashboard dropped script-src 'unsafe-inline' (blob:/srcdoc documents
	// inherit the parent policy in current engines; measured in the RFC).
	// Serving real text/html re-opens the Firefox top-level-navigation gap
	// that octet-stream+attachment closed (Firefox ignores `CSP: sandbox` on
	// top-level navigations), so the inline form is gated on
	// Sec-Fetch-Dest: iframe — every current browser stamps navigation
	// requests, top-level navigation says "document", and absence fails
	// closed. The sandbox CSP below plus the dashboard's iframe sandbox
	// attribute keep the document in an opaque origin either way.
	if r.URL.Query().Get("inline") == "1" {
		if r.Header.Get("Sec-Fetch-Dest") != "iframe" {
			httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "inline render is iframe-only"})
			return
		}
		w.Header().Set("Content-Type", mime)
	} else {
		// Legacy fetch path: octet-stream + attachment makes a direct
		// navigation download, while a dashboard fetch() still gets the
		// bytes for a client-side blob: URL.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", contentDisposition("attachment", resolved))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Belt-and-braces CSP in case Content-Type ever flips back to text/html.
	// 'unsafe-inline' 'unsafe-eval' are intentional (MathJax / KaTeX / Mermaid
	// need them; isolation comes from the opaque blob origin + iframe sandbox,
	// not CSP). img-src is data: blob: only so a same-origin-served document
	// could not probe dashboard endpoints via <img src=/api/...>.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; sandbox allow-scripts; script-src 'unsafe-inline' 'unsafe-eval' blob: data:; style-src 'unsafe-inline'; img-src data: blob:; font-src data:")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// no-store: workspace bytes must not sit in shared proxy caches. A no-store
	// response with a validator is inconsistent, so drop the ETag too.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("ETag")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// servePreview returns the first ~maxPreviewBytes of a workspace file as JSON
// for the dashboard drawer. `content` flows through httputil.WriteJSON with
// SetEscapeHTML disabled, so the CLIENT MUST assign it via textContent (or a
// sanitising renderer) — file contents are user-writable and raw innerHTML
// would be a stored-XSS sink.
func (h *Handlers) servePreview(w http.ResponseWriter, f *os.File, rootResolved, resolved string, info os.FileInfo) {
	// Mirror the serveDownload guard (full-path scan): a text-MIME .netrc /
	// .npmrc / id_rsa or `secrets/db.yaml` would otherwise be echoed in `content`.
	if isSensitiveDownloadPath(workspaceScanPath(rootResolved, resolved)) {
		httputil.WriteJSON(w, map[string]any{
			"content":   "",
			"size":      info.Size(),
			"mime":      "application/octet-stream",
			"truncated": false,
			"binary":    true,
		})
		return
	}

	size := info.Size()
	readSize := size
	truncated := false
	if readSize > maxPreviewBytes {
		readSize = maxPreviewBytes
		truncated = true
	}

	// fd plumbed in by HandleFileGet; caller owns Close.

	// Read head for MIME detection first so we can refuse non-text quickly
	// without allocating a full buffer for a potentially large binary.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	mime := detectMime(resolved, head)

	if !isTextMime(mime) {
		// Not text — clients should switch to raw/download mode. Return a
		// structured response so the drawer can render "binary file, please
		// download" without a second round-trip.
		httputil.WriteJSON(w, map[string]any{
			"content":   "",
			"size":      size,
			"mime":      mime,
			"truncated": false,
			"binary":    true,
		})
		return
	}
	// text/html, XHTML and XML MUST NOT flow through the preview JSON path:
	// WriteJSON disables HTML escaping, so <script> bytes land verbatim in the
	// response and the dashboard's esc() is one regression away from stored
	// XSS. Mirrors serveRaw's guards so preview and raw stay defence-symmetric.
	// HasPrefix covers "text/html; charset=utf-8"-style parameters.
	if strings.HasPrefix(mime, "text/html") ||
		strings.HasPrefix(mime, "application/xhtml") ||
		strings.HasPrefix(mime, "application/xml") || strings.HasPrefix(mime, "text/xml") {
		httputil.WriteJSON(w, map[string]any{
			"content":   "",
			"size":      size,
			"mime":      mime,
			"truncated": false,
			"binary":    true,
		})
		return
	}

	// Re-read from start; head may be <512 if file is tiny.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}
	buf := make([]byte, readSize)
	read, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "read failed"})
		return
	}
	buf = buf[:read]

	// Replace invalid UTF-8 so JSON encoding doesn't fail on a BOM / Latin-1
	// bytes and <pre> doesn't render garbage.
	content := string(buf)
	if !utf8.ValidString(content) {
		content = strings.ToValidUTF8(content, "\uFFFD")
	}

	httputil.WriteJSON(w, map[string]any{
		"content":   content,
		"size":      size,
		"mime":      mime,
		"truncated": truncated,
		"binary":    false,
	})
}
func (h *Handlers) serveRaw(w http.ResponseWriter, r *http.Request, f *os.File, rootResolved, resolved string, info os.FileInfo) {
	// Same sensitive-name guard (full-path scan) as servePreview / serveDownload:
	// .env / id_rsa / .npmrc sniff as text/plain and would pass isTextMime.
	if isSensitiveDownloadPath(workspaceScanPath(rootResolved, resolved)) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "preview blocked for sensitive file name"})
		return
	}
	if info.Size() > maxRawBytes {
		httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline preview; use download mode"})
		return
	}

	// fd plumbed in by HandleFileGet; caller owns Close.

	// Sniff MIME from the head so images aren't served as octet-stream.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	mime := detectMime(resolved, head[:n])
	if !isRawPreviewMime(mime) && !isTextMime(mime) {
		// Refuse: force the client into download mode rather than streaming
		// arbitrary binary as "inline". Otherwise a .exe linked from a
		// workspace could auto-execute in IE-likes / old Safari.
		httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "mime not supported for inline preview"})
		return
	}
	// text/html, image/svg+xml, XHTML and XML must never be served inline:
	// Firefox ignores the HTTP CSP sandbox directive and a direct navigation
	// renders the document same-origin with full cookie access (SVG and XHTML
	// execute <script> too). HasPrefix so parameterised detector output
	// ("image/svg+xml; charset=utf-8") still trips the guard.
	if strings.HasPrefix(mime, "text/html") || strings.HasPrefix(mime, "image/svg+xml") ||
		strings.HasPrefix(mime, "application/xhtml") ||
		strings.HasPrefix(mime, "application/xml") || strings.HasPrefix(mime, "text/xml") ||
		// text/markdown too: a UA (or future sniffer) that HTML-renders it faces
		// the same top-level-navigation risk; markdown only reaches the browser
		// via the sanitised renderer (servePreview / renderMd).
		strings.HasPrefix(mime, "text/markdown") {
		httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "inline preview disabled for this type; use download mode"})
		return
	}
	// PDFs can embed JavaScript external viewers execute, and CSP sandbox does
	// not apply to top-level navigation; serve them as explicit attachments.
	if mime == "application/pdf" {
		// Hand off the same fd; HandleFileGet's deferred Close stays the owner.
		h.serveDownload(w, r, f, rootResolved, resolved, info)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", contentDisposition("inline", resolved))
	// CSP: sandbox blocks script execution / form submission in a malicious
	// SVG while it still renders; default-src 'none' blocks remote references.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; style-src 'unsafe-inline'; img-src 'self' data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Cross-Origin-Resource-Policy blocks cross-origin <img>/<iframe> embedding
	// of previews, closing the onload dimension / timing side-channel.
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	// no-store: workspace bytes must not persist in shared proxy or browser
	// caches under no-auth deployments.
	w.Header().Set("Cache-Control", "no-store")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}
func (h *Handlers) serveDownload(w http.ResponseWriter, r *http.Request, f *os.File, rootResolved, resolved string, info os.FileInfo) {
	// Deny credential-bearing files on the explicit download path too (full-path
	// scan blocks `secrets/db.yaml`, `.ssh/foo` etc.).
	if isSensitiveDownloadPath(workspaceScanPath(rootResolved, resolved)) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "file type not downloadable"})
		return
	}

	// fd plumbed in by HandleFileGet (or serveRaw's PDF branch); caller owns
	// Close. serveRaw may have advanced the fd, so rewind first.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", resolved))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// Same rationale as serveRaw: no shared-proxy caching of workspace bytes.
	w.Header().Set("Cache-Control", "no-store")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}
