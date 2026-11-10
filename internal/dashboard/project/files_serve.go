package project

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
)

// servePreview returns the first ~maxPreviewBytes of a workspace file as JSON
// for the dashboard drawer.
func servePreview(w http.ResponseWriter, of openedFile) {
	rd, rf := previewOf(of)
	if rf != nil {
		writeRefusal(w, rf)
		return
	}
	httputil.WriteJSON(w, previewJSON(rd))
}

// previewJSON is the preview body for rd. `content` flows through
// httputil.WriteJSON with SetEscapeHTML disabled, so the CLIENT MUST assign it
// via textContent (or a sanitising renderer) — file contents are
// user-writable and raw innerHTML would be a stored-XSS sink. Invalid UTF-8
// is replaced so JSON encoding doesn't fail on a BOM / Latin-1 bytes and
// <pre> doesn't render garbage.
func previewJSON(rd node.FileRead) map[string]any {
	content := string(rd.Data)
	if !utf8.ValidString(content) {
		content = strings.ToValidUTF8(content, "\uFFFD")
	}
	return map[string]any{
		"content":   content,
		"size":      rd.Size,
		"mime":      rd.Mime,
		"truncated": rd.Truncated,
		"binary":    rd.Binary,
	}
}

// previewOf reads the preview of an opened file: up to maxPreviewBytes of
// text, or only the size and MIME (Binary) for anything that must not be
// echoed as text.
func previewOf(of openedFile) (node.FileRead, *fileRefusal) {
	f, size := of.f, of.info.Size()
	// Mirror the download guard (full-path scan): a text-MIME .netrc /
	// .npmrc / id_rsa or `secrets/db.yaml` would otherwise be echoed in `content`.
	if isSensitiveDownloadPath(workspaceScanPath(of.rootResolved, of.resolved)) {
		return node.FileRead{Size: size, Mime: "application/octet-stream", Binary: true}, nil
	}

	readSize := size
	truncated := false
	if readSize > maxPreviewBytes {
		readSize = maxPreviewBytes
		truncated = true
	}

	// Read head for MIME detection first so we can refuse non-text quickly
	// without allocating a full buffer for a potentially large binary.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	mime := detectMime(of.resolved, head)

	// Not text — clients should switch to raw/download mode; the structured
	// answer lets the drawer render "binary file, please download" without a
	// second round-trip. text/html, XHTML and XML MUST NOT flow through the
	// preview JSON path either: WriteJSON disables HTML escaping, so <script>
	// bytes land verbatim in the response and the dashboard's esc() is one
	// regression away from stored XSS. Mirrors the raw guards so preview and
	// raw stay defence-symmetric. HasPrefix covers "text/html;
	// charset=utf-8"-style parameters.
	if !isTextMime(mime) ||
		strings.HasPrefix(mime, "text/html") ||
		strings.HasPrefix(mime, "application/xhtml") ||
		strings.HasPrefix(mime, "application/xml") || strings.HasPrefix(mime, "text/xml") {
		return node.FileRead{Size: size, Mime: mime, Binary: true}, nil
	}

	// Re-read from start; head may be <512 if file is tiny.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return node.FileRead{}, refuse(http.StatusInternalServerError, "seek failed")
	}
	buf := make([]byte, readSize)
	read, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return node.FileRead{}, refuse(http.StatusInternalServerError, "read failed")
	}
	return node.FileRead{Size: size, Mime: mime, Truncated: truncated, Data: buf[:read]}, nil
}

// segmentLimit is a file_read limit clamped to node.ReverseFileChunk.
func segmentLimit(limit int) int {
	if limit <= 0 || limit > node.ReverseFileChunk {
		return node.ReverseFileChunk
	}
	return limit
}

// previewSegment cuts rd's preview to the limit bytes from offset; the
// primary reassembles it.
func previewSegment(rd node.FileRead, offset int64, limit int) (node.FileRead, *fileRefusal) {
	if offset < 0 || offset > int64(len(rd.Data)) {
		return node.FileRead{}, refuse(http.StatusBadRequest, "invalid offset")
	}
	rd.Offset = offset
	rd.Data = rd.Data[offset:min(offset+int64(segmentLimit(limit)), int64(len(rd.Data)))]
	return rd, nil
}

// readSegment reads up to limit bytes (at most node.ReverseFileChunk) from
// offset for a primary streaming the file in raw, render or download mode;
// the primary sniffs the MIME and applies those modes' gates itself.
func readSegment(of openedFile, offset int64, limit int) (node.FileRead, *fileRefusal) {
	// Every streaming mode refuses credential-bearing names.
	if isSensitiveDownloadPath(workspaceScanPath(of.rootResolved, of.resolved)) {
		return node.FileRead{}, refuse(http.StatusForbidden, "file type not downloadable")
	}
	size := of.info.Size()
	if offset < 0 || offset > size {
		return node.FileRead{}, refuse(http.StatusBadRequest, "invalid offset")
	}
	buf := make([]byte, min(int64(segmentLimit(limit)), size-offset))
	n, err := of.f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return node.FileRead{}, refuse(http.StatusInternalServerError, "read failed")
	}
	return node.FileRead{Size: size, Offset: offset, Data: buf[:n]}, nil
}

// sensitiveStreamRefusal is the 403 each streaming mode answers for a
// credential-bearing name.
var sensitiveStreamRefusal = map[string]string{
	"raw":      "preview blocked for sensitive file name",
	"render":   "render blocked for sensitive file name",
	"download": "file type not downloadable",
}

// serveStream serves an opened file in raw, render or download mode.
func serveStream(w http.ResponseWriter, r *http.Request, of openedFile, mode string) {
	// Refuse credential-bearing names (full-path scan) in every mode, even
	// when the bytes sniff as an image or HTML: .env / id_rsa / .npmrc sniff
	// as text/plain, and `secrets/db.yaml` or `.ssh/foo` sit under a
	// sensitive directory.
	if isSensitiveDownloadPath(workspaceScanPath(of.rootResolved, of.resolved)) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": sensitiveStreamRefusal[mode]})
		return
	}
	// Sniff MIME from the head so images aren't served as octet-stream; the
	// fd was opened once by HandleFileGet (O_NOFOLLOW), which owns Close.
	mime := ""
	if mode != "download" {
		head := make([]byte, 512)
		n, _ := io.ReadFull(of.f, head)
		mime = detectMime(of.resolved, head[:n])
	}
	if _, err := of.f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}
	if !prepareStream(w, r, mode, of.resolved, mime, of.info.Size()) {
		return
	}
	http.ServeContent(w, r, filepath.Base(of.resolved), of.info.ModTime(), of.f)
}

// prepareStream applies the size and type gates of a raw, render or download
// read of a file named name, sniffed as mime, and sets the headers that pin
// how the browser may treat its bytes; false means it wrote the refusal. The
// caller has refused sensitive names and streams the bytes itself, so a
// remote node's file goes out under this host's gates and headers.
func prepareStream(w http.ResponseWriter, r *http.Request, mode, name, mime string, size int64) bool {
	switch mode {
	case "raw":
		if size > maxRawBytes {
			httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline preview; use download mode"})
			return false
		}
		if !isRawPreviewMime(mime) && !isTextMime(mime) {
			// Refuse: force the client into download mode rather than streaming
			// arbitrary binary as "inline". Otherwise a .exe linked from a
			// workspace could auto-execute in IE-likes / old Safari.
			httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "mime not supported for inline preview"})
			return false
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
			// via the sanitised renderer (preview / renderMd).
			strings.HasPrefix(mime, "text/markdown") {
			httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "inline preview disabled for this type; use download mode"})
			return false
		}
		// A PDF can embed JavaScript an outside viewer runs, and Firefox ignores
		// CSP sandbox on a top-level navigation, so only the dashboard's preview
		// frame gets one inline (Sec-Fetch-Dest: iframe, absence fails closed,
		// as render's inline form); anything else downloads it. The browsers'
		// PDF viewers refuse a frame with a sandbox attribute (Chrome shows a
		// blocked page), so that frame carries none: the CSP sandbox below
		// keeps the document in an opaque origin and frame-ancestors keeps it
		// in the dashboard.
		if mime == "application/pdf" && r.Header.Get("Sec-Fetch-Dest") != "iframe" {
			return prepareStream(w, r, "download", name, mime, size)
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Disposition", contentDisposition("inline", name))
		// CSP: sandbox blocks script execution / form submission in a malicious
		// SVG while it still renders; default-src 'none' blocks remote references.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Cross-Origin-Resource-Policy blocks cross-origin <img>/<iframe> embedding
		// of previews, closing the onload dimension / timing side-channel.
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		// no-store: workspace bytes must not persist in shared proxy or browser
		// caches under no-auth deployments.
		w.Header().Set("Cache-Control", "no-store")

	case "render":
		// render streams a workspace .html / .svg so the dashboard can embed it
		// in a sandboxed iframe (coverage reports, Playwright trace, generated
		// SVG diagrams). Workspace files are untrusted (the CLI can write
		// <script> into any of them) and rendering them same-origin is stored
		// XSS. Firefox ignores the `CSP: sandbox` header on top-level
		// navigation and X-Frame-Options only covers embedding, so the default
		// answer is application/octet-stream + attachment: a direct navigation
		// downloads, while the dashboard fetch() wraps the bytes in a Blob
		// (opaque origin). Relative resources inside the document do not
		// resolve.
		if size > maxRawBytes {
			httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline render; use download mode"})
			return false
		}
		// Normalize to base MIME (strip charset params) before whitelist check.
		// detectMime returns "text/html; charset=utf-8" for real HTML payloads,
		// which must still match the "text/html" gate.
		base := mime
		if i := strings.Index(mime, ";"); i > 0 {
			base = strings.TrimSpace(mime[:i])
		}
		// Strict whitelist — only HTML/XHTML and SVG flow through render. PDF,
		// raster images, and text route through their dedicated modes.
		// detectMime pins .svg to image/svg+xml regardless of byte sniff, so an
		// attacker cannot reach this branch with non-SVG bytes by renaming a
		// .html file to .svg — the extension is authoritative for SVG.
		if base != "text/html" && base != "application/xhtml+xml" && base != "image/svg+xml" {
			httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "render mode supports HTML and SVG only; use preview/raw/download for other types"})
			return false
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
				return false
			}
			w.Header().Set("Content-Type", mime)
		} else {
			// Legacy fetch path: octet-stream + attachment makes a direct
			// navigation download, while a dashboard fetch() still gets the
			// bytes for a client-side blob: URL.
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
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

	case "download":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		// Same rationale as raw: no shared-proxy caching of workspace bytes.
		w.Header().Set("Cache-Control", "no-store")
	}
	return true
}
