package project

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// Directory-level HTML preview (planner RFC §10.2 ③, §15.1 W1): an HTML file
// is served together with the directory holding it under
// /api/preview/<token>/<rel>, so its relative CSS, JS and images load. The
// dashboard frames it with sandbox="allow-scripts" and no allow-same-origin,
// so the page runs in an opaque origin: it cannot read the dashboard's
// cookie, storage or DOM. Its subresource requests therefore carry no
// dashboard credentials, and the token in the path is the credential: an
// HMAC over (root, dir, expiry) with a per-process key, minted by POST
// /api/preview behind the dashboard's own auth. A token reads only below its
// dir, only the web types in previewTypes, under every gate HandleFileGet
// applies (path containment, symlink escape, restricted roots, sensitive
// names, size), and never as a top-level document.

const (
	// previewTTL is how long a minted preview link stays valid.
	previewTTL = time.Hour
	// maxPreviewTokenLen bounds the token before any decoding: a project
	// name or session key plus a dir of at most maxExistsPathLen.
	maxPreviewTokenLen = 4096
	// previewMACContext separates these MACs from any other use of the key.
	previewMACContext = "naozhi-preview-v1\n"
)

// previewKey signs preview tokens. Per process: a restart invalidates every
// link, which the dashboard re-mints on the next open.
var previewKey = func() []byte {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable for previewKey: " + err.Error())
	}
	return b[:]
}()

// previewGrant is what a preview token grants: reads below dir of root
// until exp.
type previewGrant struct {
	// root is a project name or a session:<key> root, as the file API
	// takes it; it is resolved again on every read.
	root string
	// dir is slash-separated and relative to root, "" for root itself.
	dir string
	// exp is the expiry, unix seconds.
	exp int64
}

func previewMAC(payload string) []byte {
	m := hmac.New(sha256.New, previewKey)
	m.Write([]byte(previewMACContext))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// signPreview encodes g as <payload>.<mac>, both base64url. The payload is
// exp, root and dir separated by newlines; root is a validated project name
// or session key and holds none, and dir comes last, so a newline in a
// directory name still parses.
func signPreview(g previewGrant) string {
	payload := strconv.FormatInt(g.exp, 10) + "\n" + g.root + "\n" + g.dir
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(previewMAC(payload))
}

// parsePreview verifies tok and returns its grant; false for a malformed,
// forged or expired token.
func parsePreview(tok string, now time.Time) (previewGrant, bool) {
	if tok == "" || len(tok) > maxPreviewTokenLen {
		return previewGrant{}, false
	}
	p64, m64, ok := strings.Cut(tok, ".")
	if !ok {
		return previewGrant{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(p64)
	if err != nil {
		return previewGrant{}, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(m64)
	if err != nil || !hmac.Equal(mac, previewMAC(string(payload))) {
		return previewGrant{}, false
	}
	parts := strings.SplitN(string(payload), "\n", 3)
	if len(parts) != 3 {
		return previewGrant{}, false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || now.Unix() >= exp {
		return previewGrant{}, false
	}
	return previewGrant{root: parts[1], dir: parts[2], exp: exp}, true
}

// previewTypes maps the extensions a preview page may load to the
// Content-Type they are served with; anything else answers 404, so a page's
// script cannot read the directory's source, configs or databases.
var previewTypes = map[string]string{
	".html":    "text/html; charset=utf-8",
	".htm":     "text/html; charset=utf-8",
	".xhtml":   "application/xhtml+xml; charset=utf-8",
	".css":     "text/css; charset=utf-8",
	".js":      "text/javascript; charset=utf-8",
	".mjs":     "text/javascript; charset=utf-8",
	".map":     "application/json; charset=utf-8",
	".json":    "application/json; charset=utf-8",
	".geojson": "application/json; charset=utf-8",
	".csv":     "text/csv; charset=utf-8",
	".tsv":     "text/tab-separated-values; charset=utf-8",
	".txt":     "text/plain; charset=utf-8",
	".md":      "text/markdown; charset=utf-8",
	".xml":     "application/xml; charset=utf-8",
	".svg":     "image/svg+xml",
	".png":     "image/png",
	".jpg":     "image/jpeg",
	".jpeg":    "image/jpeg",
	".gif":     "image/gif",
	".webp":    "image/webp",
	".avif":    "image/avif",
	".ico":     "image/x-icon",
	".bmp":     "image/bmp",
	".woff":    "font/woff",
	".woff2":   "font/woff2",
	".ttf":     "font/ttf",
	".otf":     "font/otf",
	".wasm":    "application/wasm",
	".mp4":     "video/mp4",
	".webm":    "video/webm",
	".mp3":     "audio/mpeg",
	".wav":     "audio/wav",
	".ogg":     "audio/ogg",
}

// previewHostRe is the Host a preview CSP may name; anything else (a forged
// Host with ';' would add directives) leaves the policy without a source.
var previewHostRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:.]+\](:[0-9]+)?$`)

// previewCSP is the policy of every preview response: scripts, styles,
// images, fonts, media and fetches only from this token's own prefix (both
// schemes, so a TLS-terminating proxy needs no setting), inline script and
// eval for the diagram libraries such pages bundle, no forms, no <base>, and
// only the dashboard may frame it. The sandbox directive keeps the document
// in an opaque origin even if it were embedded without the iframe attribute.
// A path-qualified source rather than 'self': 'self' would also admit the
// dashboard's own /static and /api, and WebKit does not match 'self' for
// fetches from an opaque origin.
func previewCSP(host, token string) string {
	src := "'none'"
	if previewHostRe.MatchString(host) {
		prefix := host + "/api/preview/" + token + "/"
		src = "http://" + prefix + " https://" + prefix
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + src + " 'unsafe-inline' 'unsafe-eval'",
		"style-src " + src + " 'unsafe-inline'",
		"img-src " + src + " data: blob:",
		"font-src " + src + " data:",
		"media-src " + src + " data: blob:",
		"connect-src " + src,
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors 'self'",
		"sandbox allow-scripts",
	}, "; ")
}

func (h *Handlers) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// rootArgs splits a grant's root back into browseRootOf's two arguments.
func rootArgs(root string) (sessionRoot, project string) {
	if strings.HasPrefix(root, sessionRootPrefix) {
		return root, ""
	}
	return "", root
}

// previewLinkReq is the body of POST /api/preview: the HTML file to preview,
// under a project or a root=session:<key> root as in GET /api/projects/file.
type previewLinkReq struct {
	Project string `json:"project"`
	Root    string `json:"root"`
	Path    string `json:"path"`
}

// HandlePreviewLink serves POST /api/preview: it checks that path names a
// readable HTML file and answers {url, expires_at}, the file's address under
// a fresh token for its directory. Local files only; the dashboard falls back
// to the single-file render for a remote node's.
func (h *Handlers) HandlePreviewLink(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req previewLinkReq
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	root, rf := h.browseRootOf(req.Root, req.Project)
	if rf == nil && req.Path == "" {
		rf = refuse(http.StatusBadRequest, "path is required")
	}
	if rf != nil {
		writeRefusal(w, rf)
		return
	}
	of, rf := openFile(root, req.Path, "preview", r.RemoteAddr)
	if rf != nil {
		writeRefusal(w, rf)
		return
	}
	defer of.f.Close()
	if isSensitiveDownloadPath(workspaceScanPath(of.rootResolved, of.resolved)) {
		writeRefusal(w, refuse(http.StatusNotFound, "file not found"))
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(of.f, head)
	if mime := detectMime(of.resolved, head[:n]); !strings.HasPrefix(mime, "text/html") && !strings.HasPrefix(mime, "application/xhtml") {
		writeRefusal(w, refuse(http.StatusUnsupportedMediaType, "only HTML files have a directory preview"))
		return
	}
	// openFile accepted path, so it is relative, clean of "..", and names a
	// file below the root.
	rel := filepath.ToSlash(filepath.Clean(req.Path))
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	rootRef := req.Root
	if rootRef == "" {
		rootRef = req.Project
	}
	exp := h.clock().Add(previewTTL).Unix()
	tok := signPreview(previewGrant{root: rootRef, dir: dir, exp: exp})
	httputil.WriteJSON(w, map[string]any{
		"url":        "/api/preview/" + tok + "/" + url.PathEscape(path.Base(rel)),
		"expires_at": time.Unix(exp, 0).UTC().Format(time.RFC3339),
	})
}

// HandlePreviewAsset serves GET /api/preview/{token}/{path...}: path, relative
// to the token's directory, read under the token's root. It is mounted
// outside the dashboard's auth chain because a sandboxed page's requests
// carry no dashboard credentials; the token is verified first, and every
// refusal past it is one 404 so a page probing its neighbours learns nothing.
// The token is never logged.
func (h *Handlers) HandlePreviewAsset(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	g, ok := parsePreview(tok, h.clock())
	if !ok {
		httputil.WriteJSONStatus(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired preview link"})
		return
	}
	// Only as a frame or a page's subresource: a top-level navigation would
	// render the page in the dashboard's origin in a browser that ignores the
	// CSP sandbox there (Firefox). Every current browser stamps the header;
	// absence fails closed.
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest == "" || dest == "document" {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "preview is frame-only"})
		return
	}
	notFound := refuse(http.StatusNotFound, "file not found")
	rel := r.PathValue("path")
	ctype, ok := previewTypes[strings.ToLower(path.Ext(rel))]
	if !ok || strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
		writeRefusal(w, notFound)
		return
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		writeRefusal(w, notFound)
		return
	}
	root, rf := h.browseRootOf(rootArgs(g.root))
	if rf != nil {
		writeRefusal(w, notFound)
		return
	}
	dir := filepath.FromSlash(g.dir)
	of, rf := openFile(root, filepath.Join(dir, cleaned), "preview", r.RemoteAddr)
	if rf != nil {
		writeRefusal(w, notFound)
		return
	}
	defer of.f.Close()
	// openFile kept the file below the root; a symlink inside the directory
	// may still point elsewhere in the root, outside what the token grants.
	if dir != "" {
		dirResolved, err := filepath.EvalSymlinks(filepath.Join(of.rootResolved, dir))
		if err != nil || !strings.HasPrefix(of.resolved, dirResolved+string(filepath.Separator)) {
			writeRefusal(w, notFound)
			return
		}
	}
	if isSensitiveDownloadPath(workspaceScanPath(of.rootResolved, of.resolved)) || of.info.Size() > maxRawBytes {
		writeRefusal(w, notFound)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", ctype)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", previewCSP(auth.RequestHost(r, h.deps.TrustedProxy), tok))
	hd.Set("Referrer-Policy", "no-referrer")
	// no-store: a follow refresh must see the new bytes, and the token in the
	// URL must not sit in a cache.
	hd.Set("Cache-Control", "no-store")
	// The page's module scripts and fetches are CORS requests from its opaque
	// origin. No cookie is involved and the token in the URL is the only
	// credential, so a wildcard grants nothing a holder of the URL lacks.
	hd.Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "", of.info.ModTime(), of.f)
}
