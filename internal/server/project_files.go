package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// File API size / count limits. All values are deliberately conservative so a
// misbehaving browser tab or compromised token cannot DoS the host:
//
//   - maxExistsPaths: caps the batch existence-check request body. 100 paths
//     per call matches what a single chat bubble plausibly references; anything
//     beyond that is almost certainly not user-driven.
//   - maxExistsPathLen: one path's bytes. 1 KB is ~4x the ext4 MAX_PATH so
//     crafted overlong paths are rejected before filepath.Clean.
//   - maxPreviewBytes: text preview cap. Anything larger redirects the user to
//     download; 1 MB renders in <100ms on 4G and does not crash <pre>.
//   - maxRawBytes: inline image/PDF cap. Larger files force download to avoid
//     browsers mis-managing 500MB video in memory.
const (
	maxExistsPaths   = 100
	maxExistsPathLen = 1024
	maxExistsBody    = 64 * 1024
	maxPreviewBytes  = 1 * 1024 * 1024
	maxRawBytes      = 50 * 1024 * 1024
	fileStatTimeout  = 2 * time.Second
)

// textMimePrefixes identifies MIME types safe to return as UTF-8 text in
// preview mode. http.DetectContentType tags source code as "text/plain" which
// covers most cases; JSON/YAML/XML/JS are also safe even when the detector
// returns their specific type.
var textMimeSet = map[string]struct{}{
	"application/json":          {},
	"application/javascript":    {},
	"application/xml":           {},
	"application/x-yaml":        {},
	"application/yaml":          {},
	"application/toml":          {},
	"application/x-sh":          {},
	"application/x-shellscript": {},
}

// previewableByExt lets us override the generic "application/octet-stream"
// that DetectContentType returns for most source code extensions. Without
// this, every .go/.py/.ts file would be refused preview.
var previewableByExt = map[string]string{
	".go":            "text/x-go",
	".py":            "text/x-python",
	".js":            "application/javascript",
	".mjs":           "application/javascript",
	".ts":            "application/typescript",
	".tsx":           "application/typescript",
	".jsx":           "application/javascript",
	".rs":            "text/x-rust",
	".java":          "text/x-java",
	".kt":            "text/x-kotlin",
	".kts":           "text/x-kotlin",
	".c":             "text/x-c",
	".h":             "text/x-c",
	".cc":            "text/x-c++",
	".cpp":           "text/x-c++",
	".hpp":           "text/x-c++",
	".cs":            "text/x-csharp",
	".rb":            "text/x-ruby",
	".php":           "text/x-php",
	".swift":         "text/x-swift",
	".md":            "text/markdown",
	".markdown":      "text/markdown",
	".txt":           "text/plain",
	".log":           "text/plain",
	".json":          "application/json",
	".jsonl":         "application/json",
	".yaml":          "application/yaml",
	".yml":           "application/yaml",
	".toml":          "application/toml",
	".xml":           "application/xml",
	".html":          "text/html",
	".htm":           "text/html",
	".css":           "text/css",
	".sh":            "application/x-sh",
	".bash":          "application/x-sh",
	".zsh":           "application/x-sh",
	".sql":           "text/x-sql",
	".dockerfile":    "text/plain",
	".env":           "text/plain",
	".gitignore":     "text/plain",
	".gitattributes": "text/plain",
	".makefile":      "text/x-makefile",
	".mk":            "text/x-makefile",
	".proto":         "text/x-protobuf",
	".graphql":       "text/plain",
	".gql":           "text/plain",
	".conf":          "text/plain",
	".cfg":           "text/plain",
	".ini":           "text/plain",
}

// rawPreviewMimes identifies file types the browser can render inline via <img>
// or <iframe>. Any MIME prefix here is allowed through mode=raw without forcing
// a download.
var rawPreviewMimes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml",
	"application/pdf",
}

// existsReq is the batch payload for POST /api/projects/files/exists.
type existsReq struct {
	Project string   `json:"project"`
	Node    string   `json:"node,omitempty"`
	Paths   []string `json:"paths"`
}

type existsEntry struct {
	Exists bool   `json:"exists"`
	Size   int64  `json:"size,omitempty"`
	Mime   string `json:"mime,omitempty"`
	IsDir  bool   `json:"is_dir,omitempty"`
}

// resolveProjectFile joins rel to the project's workspace path and ensures
// the result is a regular file (or symlink to one) located under projectPath
// after symlink resolution. Errors are deliberately generic so the frontend
// cannot distinguish "missing" from "outside workspace" from "symlink escape"
// via timing or message text — all collapse to a single not-found signal that
// callers either render as `exists:false` or a 404.
//
// Unlike validateWorkspace (which demands a directory), this helper accepts
// both files and directories; callers post-process via os.Stat if they need
// to distinguish.
func resolveProjectFile(projectPath, rel string) (string, error) {
	// Check empty BEFORE EvalSymlinks: filepath.EvalSymlinks("") returns
	// (".", nil) on Linux, which would silently bind resolution to the
	// process CWD and bypass the "project not configured" guard below.
	// R61-GO-1.
	if projectPath == "" {
		return "", errors.New("project not configured")
	}
	rootResolved, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", err
	}
	return resolveProjectFileWithRoot(rootResolved, rel)
}

// resolveProjectFileWithRoot is the inner half of resolveProjectFile: it
// accepts an already-resolved project root so callers iterating over many
// paths (e.g. handleFilesExists, which does up to 100 stats per request)
// don't re-EvalSymlinks the same root for every call. Callers who only
// resolve one path should use resolveProjectFile. R59-PERF-M3.
func resolveProjectFileWithRoot(rootResolved, rel string) (string, error) {
	if rootResolved == "" {
		return "", errors.New("project not configured")
	}
	if rel == "" {
		return "", errors.New("path is required")
	}
	if len(rel) > maxExistsPathLen {
		return "", errors.New("path too long")
	}
	// Reject NUL — Go os calls will error anyway but we want to fail before
	// the argument ever reaches filepath.Join.
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("invalid path")
	}
	// Reject absolute paths: `/foo` joined with projectPath silently
	// overwrites the project root on some platforms. Clients must always
	// send workspace-relative paths.
	if filepath.IsAbs(rel) {
		return "", errors.New("path must be relative")
	}
	// Clean before join so `..` segments cannot escape; the symlink-resolved
	// prefix check below is defence-in-depth, but collapsing `a/../x` up
	// front avoids calling os.Stat on obviously hostile paths at all.
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	full := filepath.Join(rootResolved, cleaned)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	// Prefix check protects against symlink escapes. A file inside the
	// project that symlinks to /etc/passwd would resolve outside rootResolved
	// and get rejected here, matching the validateWorkspace contract.
	if resolved != rootResolved &&
		!strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	return resolved, nil
}

// detectMime runs http.DetectContentType on the first 512 bytes plus an
// extension override for source code that would otherwise be tagged as
// application/octet-stream.
func detectMime(resolved string, head []byte) string {
	mime := http.DetectContentType(head)
	ext := strings.ToLower(filepath.Ext(resolved))
	// SVGs starting with `<?xml ... ?>` sniff as text/xml, which isTextMime
	// accepts — serveRaw's "image/svg+xml" block would then be bypassed and
	// the browser would render the SVG as same-origin XML (script execution
	// on top-level navigation). Pin .svg to image/svg+xml before any generic
	// sniff result can leak through. Attachment disposition in serveRaw then
	// forces a download; no inline rendering regardless of underlying bytes.
	if ext == ".svg" {
		return "image/svg+xml"
	}
	// Base name override for extensionless files like Dockerfile / Makefile.
	if ext == "" {
		base := strings.ToLower(filepath.Base(resolved))
		if v, ok := previewableByExt["."+base]; ok {
			return v
		}
	}
	if strings.HasPrefix(mime, "text/plain") || strings.HasPrefix(mime, "application/octet-stream") {
		if v, ok := previewableByExt[ext]; ok {
			return v
		}
	}
	return mime
}

func isTextMime(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	base := mime
	if i := strings.Index(mime, ";"); i > 0 {
		base = strings.TrimSpace(mime[:i])
	}
	_, ok := textMimeSet[base]
	return ok
}

func isRawPreviewMime(mime string) bool {
	base := mime
	if i := strings.Index(mime, ";"); i > 0 {
		base = strings.TrimSpace(mime[:i])
	}
	for _, p := range rawPreviewMimes {
		if base == p {
			return true
		}
	}
	return false
}

// sanitizeDownloadName strips control characters and path separators from the
// filename used in Content-Disposition. A raw filename can smuggle CR/LF into
// response headers (HTTP response splitting) or cause Windows to treat the
// download as a path reference. filepath.Base handles the path; we still need
// to scrub control bytes the base retains.
func sanitizeDownloadName(p string) string {
	base := filepath.Base(p)
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop controls
		case r == '"', r == '\\':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "download"
	}
	return out
}

// contentDisposition builds a Content-Disposition header value respecting
// RFC 6266 / RFC 5987. Filenames that contain non-ASCII codepoints (common
// for Chinese, Japanese, emoji-in-name, etc.) must be encoded via the
// `filename*=UTF-8”...` form so intermediaries with a stricter HTTP parser
// don't mangle or reject the response. Pure-ASCII names keep the simpler
// quoted form so curl / wget / old browsers continue to display them as-is.
// R71-SEC-M1.
func contentDisposition(kind, resolved string) string {
	name := sanitizeDownloadName(resolved)
	ascii := true
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return fmt.Sprintf(`%s; filename="%s"`, kind, name)
	}
	// Emit both forms: ASCII fallback (with non-ASCII stripped to '_') for
	// legacy clients + RFC 5987 UTF-8 form for modern browsers.
	asciiFallback := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 0x80 {
			asciiFallback = append(asciiFallback, '_')
		} else {
			asciiFallback = append(asciiFallback, c)
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, asciiFallback, url.PathEscape(name))
}

// POST /api/projects/files/exists
//
// Batch stat up to maxExistsPaths paths under the project workspace. Used by
// the dashboard to decide whether a path mentioned in a message bubble should
// get a "preview / download" button pair. Paths that don't resolve or fall
// outside the workspace come back as {exists:false} rather than an error, so
// the frontend can treat validation as a cheap yes/no.
func (h *ProjectHandlers) handleFilesExists(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxExistsBody)
	var req existsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Debug("files exists: decode failed", "err", err)
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if req.Project == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}
	if len(req.Paths) == 0 {
		writeJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}
	if len(req.Paths) > maxExistsPaths {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("too many paths (max %d)", maxExistsPaths)})
		return
	}

	p := h.projectMgr.Get(req.Project)
	if p == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), fileStatTimeout)
	defer cancel()

	// Resolve the project root once up front. The previous statRel called
	// resolveProjectFile per path, which EvalSymlinks'd the same project
	// root up to maxExistsPaths (100) times on every batch. With the root
	// pre-resolved, each path costs a single EvalSymlinks on the joined
	// target. On slow filesystems this was the leading contributor to the
	// fileStatTimeout budget. R59-PERF-M3.
	//
	// Check empty BEFORE EvalSymlinks: EvalSymlinks("") returns (".", nil)
	// on Linux which would bind path resolution to the process CWD.
	// R61-GO-1.
	if p.Path == "" {
		writeJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}
	rootResolved, err := filepath.EvalSymlinks(p.Path)
	if err != nil {
		// Treat an unresolvable project root as "nothing exists" so the
		// frontend renders plain text fallback. Matching the existing
		// contract: errors collapse to {exists:false}.
		writeJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}

	results := make(map[string]existsEntry, len(req.Paths))
	for _, rel := range req.Paths {
		if err := ctx.Err(); err != nil {
			// Timeout: return whatever we've collected so far; remaining
			// entries default to {exists:false}. This is safer than 500 —
			// the frontend treats unknowns as "no button", preserving the
			// text-only fallback.
			break
		}
		results[rel] = statRelWithRoot(rootResolved, rel)
	}

	writeJSON(w, map[string]any{"results": results})
}

// statRelWithRoot stats a single project-relative path and returns the
// metadata the dashboard needs to decide preview vs download. Errors
// collapse to {exists:false}; the frontend never sees which validation
// stage rejected the path, matching the validateWorkspace contract.
// Callers must pass an already-resolved project root so batch call sites
// don't pay N × EvalSymlinks(rootResolved). R59-PERF-M3.
func statRelWithRoot(rootResolved, rel string) existsEntry {
	resolved, err := resolveProjectFileWithRoot(rootResolved, rel)
	if err != nil {
		return existsEntry{Exists: false}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return existsEntry{Exists: false}
	}
	if info.IsDir() {
		return existsEntry{Exists: true, IsDir: true, Size: info.Size()}
	}

	// Peek the first 512 bytes for MIME detection. On small files this is
	// the entire content; reading it here avoids a second open in the
	// preview handler later. We intentionally do NOT cache this across
	// calls — mtime changes would stale the cache and the per-call cost is
	// dominated by the open, not the read.
	mime := ""
	if info.Size() > 0 {
		f, openErr := os.Open(resolved)
		if openErr == nil {
			head := make([]byte, 512)
			n, _ := io.ReadFull(f, head)
			f.Close()
			mime = detectMime(resolved, head[:n])
		}
	} else {
		mime = "text/plain"
	}
	return existsEntry{Exists: true, Size: info.Size(), Mime: mime}
}

// GET /api/projects/file?project=X&path=Y&mode=preview|raw|download
//
// Returns the file contents in one of three shapes:
//   - preview: JSON {content, truncated, size, mime}. Text-only, capped to
//     maxPreviewBytes. Invalid UTF-8 is replaced with U+FFFD so <pre> renders
//     safely.
//   - raw: binary stream with Content-Type=mime, Content-Disposition=inline.
//     For images/PDF in <img>/<iframe>. Capped to maxRawBytes.
//   - download: binary stream with Content-Type=application/octet-stream,
//     Content-Disposition=attachment. No body size cap (but http.ServeContent
//     handles Range so the client can resume).
//
// ETag is "<size>-<mtime-nanos>" in all modes. 304 on If-None-Match.
func (h *ProjectHandlers) handleFileGet(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	project := r.URL.Query().Get("project")
	path := r.URL.Query().Get("path")
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "preview"
	}
	if mode != "preview" && mode != "raw" && mode != "download" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid mode"})
		return
	}
	if project == "" || path == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project and path are required"})
		return
	}

	p := h.projectMgr.Get(project)
	if p == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	resolved, err := resolveProjectFile(p.Path, path)
	if err != nil {
		// os.ErrNotExist (valid but missing) vs outside-workspace collapse to
		// 404 — an attacker probing paths gets the same signal either way.
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	etag := fmt.Sprintf(`"%d-%d"`, info.Size(), info.ModTime().UnixNano())
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	// Private because responses may contain workspace source; don't let
	// shared proxies cache them across users even on the same origin.
	w.Header().Set("Cache-Control", "private, max-age=60")

	switch mode {
	case "preview":
		h.servePreview(w, resolved, info)
	case "raw":
		h.serveRaw(w, r, resolved, info)
	case "download":
		h.serveDownload(w, r, resolved, info)
	}
}

// servePreview returns the first ~maxPreviewBytes of a workspace file as JSON
// so the dashboard drawer can render it with syntax highlighting. The `content`
// field flows through writeJSON with SetEscapeHTML disabled, so the CLIENT
// MUST assign it via `textContent` or pass it through DOMPurify/a whitelist
// renderer before `innerHTML`. File contents are user-writable — Claude CLI
// tools create/edit files arbitrarily — so raw innerHTML would be a stored-XSS
// sink. dashboard.js currently uses `<pre><code>esc(content)</code></pre>`
// with esc() HTML-escaping the payload, satisfying this contract. R71-SEC-L1.
func (h *ProjectHandlers) servePreview(w http.ResponseWriter, resolved string, info os.FileInfo) {
	size := info.Size()
	readSize := size
	truncated := false
	if readSize > maxPreviewBytes {
		readSize = maxPreviewBytes
		truncated = true
	}

	f, err := os.Open(resolved)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "open failed"})
		return
	}
	defer f.Close()

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
		writeJSON(w, map[string]any{
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
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}
	buf := make([]byte, readSize)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "read failed"})
		return
	}
	buf = buf[:read]

	// Replace invalid UTF-8 so JSON encoding doesn't fail and <pre> doesn't
	// render as garbled bytes. A text file with a BOM or Latin-1 bytes would
	// otherwise abort the entire response.
	content := string(buf)
	if !utf8.ValidString(content) {
		content = strings.ToValidUTF8(content, "\uFFFD")
	}

	writeJSON(w, map[string]any{
		"content":   content,
		"size":      size,
		"mime":      mime,
		"truncated": truncated,
		"binary":    false,
	})
}

func (h *ProjectHandlers) serveRaw(w http.ResponseWriter, r *http.Request, resolved string, info os.FileInfo) {
	if info.Size() > maxRawBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline preview; use download mode"})
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "open failed"})
		return
	}
	defer f.Close()

	// Sniff MIME from file head so we don't hand the browser octet-stream for
	// images; http.ServeContent reads content-type from w.Header() if set.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	mime := detectMime(resolved, head[:n])
	if !isRawPreviewMime(mime) && !isTextMime(mime) {
		// Refuse: force the client into download mode rather than streaming
		// arbitrary binary as "inline". Otherwise a .exe linked from a
		// workspace could auto-execute in IE-likes / old Safari.
		writeJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "mime not supported for inline preview"})
		return
	}
	// text/html is same-origin HTML served from the dashboard. Firefox
	// ignores the HTTP CSP sandbox directive, and even where it works, a
	// direct navigation to this URL in a new tab renders the document
	// with full access to dashboard cookies. Force download mode to
	// prevent stored-XSS from a workspace file a tool might have written.
	//
	// image/svg+xml has the same problem: SVG can embed <script> and runs
	// with full same-origin privileges on top-level navigation. The CSP
	// `sandbox` directive only applies to iframe embedding, not to the
	// tab the user lands on when clicking the preview URL. SVGs must
	// only reach the browser as attachments.
	// HasPrefix on both so a future detector output of "image/svg+xml; charset=utf-8"
	// (or any parameter) still trips the guard instead of falling through to inline.
	if strings.HasPrefix(mime, "text/html") || strings.HasPrefix(mime, "image/svg+xml") {
		writeJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "inline preview disabled for this type; use download mode"})
		return
	}
	// PDFs can embed JavaScript that Adobe Reader (as an external plugin)
	// executes in its own context. The HTTP `Content-Security-Policy: sandbox`
	// directive is only enforced on iframe-embedded documents, not top-level
	// navigations; opening /api/projects/file?...mode=raw in a new tab on a
	// malicious PDF would bypass the sandbox entirely. Route PDFs to the
	// download path so the browser / OS handler treats them as explicit
	// attachments. R71-SEC-M2.
	if mime == "application/pdf" {
		h.serveDownload(w, r, resolved, info)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", contentDisposition("inline", resolved))
	// CSP on raw responses: a malicious SVG could embed <script>; the sandbox
	// directive blocks script execution and form submission while still
	// allowing the image to render. default-src 'none' means any referenced
	// URL in the SVG (remote <image>, external fonts) is also blocked.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; style-src 'unsafe-inline'; img-src 'self' data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Cross-Origin-Resource-Policy prevents cross-origin <img>/<iframe>
	// embedding of workspace previews. Combined with SameSite cookies this
	// closes the side-channel where an attacker-controlled origin embeds a
	// preview URL via <img src> and reads onload dimensions / timing while
	// the user is authenticated. R61-SEC-3.
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	// Raw/download paths serve workspace file content that may be sensitive.
	// `no-store` prevents both intermediate proxies and the browser cache
	// from persisting the bytes, closing a cross-user-reuse gap on shared
	// proxies under no-auth deployments. R71-SEC-L2.
	w.Header().Set("Cache-Control", "no-store")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

func (h *ProjectHandlers) serveDownload(w http.ResponseWriter, r *http.Request, resolved string, info os.FileInfo) {
	f, err := os.Open(resolved)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "open failed"})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", resolved))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// Same rationale as serveRaw: workspace file bytes must not sit in
	// shared proxy caches under no-auth deployments. R71-SEC-L2.
	w.Header().Set("Cache-Control", "no-store")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}
