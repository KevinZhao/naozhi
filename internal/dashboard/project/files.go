package project

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// FileETagSalt is a per-process random byte string mixed into the ETag hash
// for HandleFileGet so an authenticated caller cannot use the If-None-Match
// 304-vs-200 oracle to recover (size, mtime) from candidate ETags (#418).
// Regenerating it per process invalidates client caches on restart, which is
// acceptable (private files, max-age=60). crypto/rand failure at init is
// fatal rather than serving probe-able ETags.
var FileETagSalt = func() []byte {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fatal: serving unsalted ETags would silently regress the security
		// property, and a package-level initialiser cannot return an error.
		panic("crypto/rand unavailable for FileETagSalt: " + err.Error())
	}
	return b[:]
}()

// File API size / count limits, deliberately conservative so a misbehaving
// tab or compromised token cannot DoS the host: maxExistsPaths caps the batch
// existence-check body (what one chat bubble plausibly references),
// maxExistsPathLen rejects overlong paths before filepath.Clean,
// maxPreviewBytes caps text preview and maxRawBytes caps inline image/PDF
// (anything larger is redirected to download).
const (
	maxExistsPaths   = 100
	maxExistsPathLen = 1024
	maxExistsBody    = 64 * 1024
	maxPreviewBytes  = 1 * 1024 * 1024
	maxRawBytes      = 50 * 1024 * 1024
	fileStatTimeout  = 2 * time.Second
)

// publicTmpProject is a reserved pseudo-project name that maps onto /tmp so
// the dashboard can preview/download chat-mentioned /tmp paths without
// registering /tmp as a project. Any authenticated dashboard user can then
// read non-credential files under /tmp — acceptable for a single-operator
// dashboard, not multi-tenant — so it is gated by Handlers.publicTmpEnabled
// (`server.public_tmp_enabled`, default false) (#646). Symlink escapes are
// still rejected by resolveProjectFileWithRoot and the credential allowlist
// still applies. The handler intercepts the name before the projectMgr
// lookup so a real project with this name cannot shadow it.
const (
	publicTmpProject = "__public_tmp__"
	publicTmpRoot    = "/tmp"
)

// processEUID is the naozhi process EUID, captured once so
// isPublicTmpForeignPrivate stays syscall-free and tests can override it.
var processEUID = uint32(os.Geteuid())

// isPublicTmpForeignPrivate refuses /tmp files that are owner-private (no
// group/world bits) AND owned by a UID other than the naozhi process (#831).
// Linux DAC checks the running process, not the dashboard caller, so without
// this gate a dashboard user could read another OS user's 0600 files under
// /tmp. World/group-readable and same-UID files stay accessible. Reads the
// already-Lstat'd FileInfo, so zero syscalls on the hot path.
func isPublicTmpForeignPrivate(info os.FileInfo) bool {
	uid, ok := fileOwnerUID(info)
	if !ok {
		// Cannot read the owner UID (non-Unix or stub FileInfo): fail closed.
		// Production is always Linux where ok is true.
		return true
	}
	if uid == processEUID {
		return false
	}
	const groupOrWorld = 0o077
	return info.Mode().Perm()&groupOrWorld == 0
}

// publicTmpDeniedSuffixes lists basename suffixes never served through
// __public_tmp__ even when world/group readable (#1330): /tmp holds
// world-accessible Unix sockets (ssh-agent, gpg-agent, postgres/redis IPC)
// whose payload is authentication state, .pid files aid kill/ptrace probes,
// and core/crash dumps are memory snapshots. Matched on the basename of the
// resolved path so a directory component called "ssh" does not trip it.
var publicTmpDeniedSuffixes = []string{
	".sock",
	".pid",
}

// publicTmpDeniedSubstrings catches names without a known suffix
// (`ssh-agent.<pid>`, `S.gpg-agent`, the `.xauthority` MIT-MAGIC-COOKIE file,
// `.dbus-keyrings`); case-insensitive on the basename.
var publicTmpDeniedSubstrings = []string{
	"ssh",
	"gpg",
	".xauthority",
	".dbus",
}

// publicTmpDeniedPrefixes catches dump/crash artefacts whose names start
// with a known marker followed by a pid/timestamp. Matched on the
// case-insensitive basename so `core.1234` and `crash.txt` both trip.
var publicTmpDeniedPrefixes = []string{
	"core.",
	"crash.",
}

// isPublicTmpDeniedName reports whether the basename of resolved is refused by
// __public_tmp__ regardless of mode bits (see publicTmpDeniedSuffixes).
func isPublicTmpDeniedName(resolved string) bool {
	name := strings.ToLower(filepath.Base(resolved))
	if name == "" || name == "." || name == "/" {
		return false
	}
	for _, suf := range publicTmpDeniedSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	for _, sub := range publicTmpDeniedSubstrings {
		if strings.Contains(name, sub) {
			return true
		}
	}
	for _, pre := range publicTmpDeniedPrefixes {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// isPublicTmpIrregularType reports whether the file is a Unix socket, FIFO or
// device node, none of which may be served through __public_tmp__ (#1688). A
// world-readable socket with an unlisted name passes both the deny-list and
// the foreign-private gate, yet reflecting it discloses IPC payload; FIFOs and
// devices can block or leak kernel state. Independent of name and permission
// bits; zero syscalls (uses the Lstat'd FileInfo).
func isPublicTmpIrregularType(info os.FileInfo) bool {
	const irregular = os.ModeSocket | os.ModeNamedPipe | os.ModeDevice | os.ModeCharDevice
	return info.Mode()&irregular != 0
}

// textMimeSet lists non-"text/" MIME types safe to return as UTF-8 text in
// preview mode (http.DetectContentType may return these specific types).
var textMimeSet = map[string]struct{}{
	"application/json":          {},
	"application/javascript":    {},
	"application/typescript":    {},
	"application/xml":           {},
	"application/x-yaml":        {},
	"application/yaml":          {},
	"application/toml":          {},
	"application/x-sh":          {},
	"application/x-shellscript": {},
}

// previewableByExt overrides the application/octet-stream that
// DetectContentType returns for most source extensions.
var previewableByExt = map[string]string{
	".go":       "text/x-go",
	".py":       "text/x-python",
	".js":       "application/javascript",
	".mjs":      "application/javascript",
	".ts":       "application/typescript",
	".tsx":      "application/typescript",
	".jsx":      "application/javascript",
	".rs":       "text/x-rust",
	".java":     "text/x-java",
	".kt":       "text/x-kotlin",
	".kts":      "text/x-kotlin",
	".c":        "text/x-c",
	".h":        "text/x-c",
	".cc":       "text/x-c++",
	".cpp":      "text/x-c++",
	".hpp":      "text/x-c++",
	".cs":       "text/x-csharp",
	".rb":       "text/x-ruby",
	".php":      "text/x-php",
	".swift":    "text/x-swift",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".log":      "text/plain",
	".json":     "application/json",
	".jsonl":    "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".xml":      "application/xml",
	// .html / .htm intentionally NOT mapped: servePreview/serveRaw block
	// text/html after sniffing, and listing them here would let
	// mimeFromExtOnly's fast path return text/html without the sniff.
	".css":        "text/css",
	".sh":         "application/x-sh",
	".bash":       "application/x-sh",
	".zsh":        "application/x-sh",
	".sql":        "text/x-sql",
	".dockerfile": "text/plain",
	// .env intentionally NOT mapped: it falls through to DetectContentType as
	// application/octet-stream so servePreview's MIME guard rejects it.
	".gitignore":     "text/plain",
	".gitattributes": "text/plain",
	".makefile":      "text/x-makefile",
	".mk":            "text/x-makefile",
	".proto":         "text/x-protobuf",
	".graphql":       "text/plain",
	".gql":           "text/plain",
	// .conf / .cfg / .ini are deliberately previewable: authenticated users
	// already have full read access (download / raw / CLI Read), so refusing
	// preview only adds click-through cost. Secret-name blocking belongs in
	// sensitiveDownloadNames / sensitiveDownloadExts, not here.
	".conf": "text/plain",
	".cfg":  "text/plain",
	".ini":  "text/plain",
}

// rawPreviewMimes lists types the browser may render inline via <img> or
// <iframe> under mode=raw. SVG is intentionally absent: serveRaw rejects
// image/svg+xml (stored XSS via <script>), and SVG previews flow only through
// serveRender's blob-URL path.
var rawPreviewMimes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp",
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

// isClientPathRejection reports whether err is one of the well-known
// "client supplied a malformed path" rejections from resolveProjectFile, so
// the handler logs only genuine filesystem failures and a probing client
// cannot flood the logs. Matched by exact error string against the literals
// returned in this package.
func isClientPathRejection(err error) bool {
	if err == nil {
		return false
	}
	switch err.Error() {
	case "project not configured",
		"path is required",
		"path too long",
		"invalid path",
		"path must be relative",
		"path escapes workspace":
		return true
	}
	return false
}

// resolveProjectFile joins rel to the project workspace and returns the
// symlink-resolved path, which must stay under projectPath. Errors are
// deliberately generic so the frontend cannot distinguish "missing" from
// "outside workspace" from "symlink escape". Unlike validateWorkspace it
// accepts both files and directories.
func resolveProjectFile(projectPath, rel string) (string, error) {
	// Check empty BEFORE EvalSymlinks: EvalSymlinks("") returns (".", nil),
	// which would silently bind resolution to the process CWD.
	if projectPath == "" {
		return "", errors.New("project not configured")
	}
	rootResolved, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", err
	}
	return resolveProjectFileWithRoot(rootResolved, rel)
}

// resolveProjectFileWithRoot is the inner half of resolveProjectFile, taking
// an already-resolved root so batch callers (HandleFilesExists) do not
// re-EvalSymlinks the same root per path.
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
	// Reject NUL before it ever reaches filepath.Join.
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("invalid path")
	}
	// Reject absolute paths: `/foo` joined with projectPath would replace the
	// root on some platforms; clients must send workspace-relative paths.
	if filepath.IsAbs(rel) {
		return "", errors.New("path must be relative")
	}
	// Clean before join so `..` cannot escape; the prefix check below is
	// defence-in-depth, this avoids stat-ing obviously hostile paths.
	cleaned := filepath.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
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
	// Prefix check catches symlink escapes (a file symlinked to /etc/passwd
	// resolves outside rootResolved), matching the validateWorkspace contract.
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
	// SVGs starting with `<?xml ?>` sniff as text/xml, which isTextMime
	// accepts and would bypass serveRaw's image/svg+xml block. Pin .svg so
	// serveRaw's attachment disposition always forces a download.
	if ext == ".svg" {
		return "image/svg+xml"
	}
	// Pin .html / .htm to text/html here ONLY (not in previewableByExt) so
	// serveRender can route empty/short HTML that sniffs as text/plain, while
	// mimeFromExtOnly's fast path can never short-circuit the byte sniff and
	// servePreview / serveRaw still hit their text/html block gates.
	if ext == ".html" || ext == ".htm" {
		if strings.HasPrefix(mime, "text/plain") || strings.HasPrefix(mime, "application/octet-stream") {
			return "text/html"
		}
		return mime
	}
	// Base name override for extensionless files (Dockerfile / Makefile).
	// Dotfiles like ".gitignore" have filepath.Ext == basename, so look them
	// up by basename directly.
	if ext == "" {
		base := strings.ToLower(filepath.Base(resolved))
		if v, ok := previewableByExt["."+base]; ok {
			return v
		}
	} else if base := strings.ToLower(filepath.Base(resolved)); strings.HasPrefix(base, ".") && base == ext {
		if v, ok := previewableByExt[base]; ok {
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

// sanitizeDownloadName strips control characters and path separators from
// the filename used in Content-Disposition: CR/LF would enable response
// splitting, and C1 controls / bidi overrides (osutil.IsLogInjectionRune)
// survive percent-encoding to confuse intermediaries or make `foo.txt`
// render as `foo.exe` in the UI.
func sanitizeDownloadName(p string) string {
	base := filepath.Base(p)
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop C0 controls
		case osutil.IsLogInjectionRune(r):
			// drop C1 controls + bidi override / isolate + LS / PS
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

// contentDisposition builds an RFC 6266 / RFC 5987 Content-Disposition
// value: pure-ASCII names use the plain quoted form for legacy clients,
// non-ASCII names add the `filename*=UTF-8”...` form so strict
// intermediaries do not mangle them.
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

// HandleFilesExists serves POST /api/projects/files/exists: batch-stat up to
// maxExistsPaths paths under the project workspace so the dashboard can decide
// whether a path mentioned in a bubble gets preview/download buttons. Paths
// that don't resolve or fall outside the workspace come back as {exists:false}.
func (h *Handlers) HandleFilesExists(w http.ResponseWriter, r *http.Request) {
	// Rate-limit before any work: the endpoint fans out up to maxExistsPaths
	// stats within fileStatTimeout, so a post-auth attacker targeting slow NFS
	// mounts or symlink loops could tie up workers. Nil-guarded for tests.
	if h.filesExistsLimiter != nil && !h.filesExistsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "files/exists rate limit exceeded"})
		return
	}
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	r = httputil.WithMaxBytes(w, r, maxExistsBody)
	var req existsReq
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		slog.Debug("files exists: decode failed", "err", err)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if req.Project == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}
	// __public_tmp__ pseudo-project: pin rootPath to /tmp without a project
	// registration; resolveProjectFileWithRoot still guards symlink escape /
	// traversal / credential names.
	rootPath := ""
	// restrictedRoot mirrors HandleFileGet: __public_tmp__ and include_root
	// projects get the foreign-private / denied-name / irregular-type gates and
	// the credential-name filter so batch-exists cannot enumerate what GET refuses.
	restrictedRoot := false
	if req.Project == publicTmpProject && h.publicTmpEnabled {
		rootPath = publicTmpRoot
		restrictedRoot = true
	} else {
		// Same validateProjectName trust-boundary gate as every other
		// /api/projects path; a future log of the miss path must not become a
		// log-injection hole.
		if err := validateProjectName(req.Project); err != nil {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid project name"})
			return
		}
	}
	if len(req.Paths) == 0 {
		httputil.WriteJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}
	if len(req.Paths) > maxExistsPaths {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("too many paths (max %d)", maxExistsPaths)})
		return
	}

	if rootPath == "" {
		p := h.projectMgr.Get(req.Project)
		if p == nil {
			httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		rootPath = p.Path
		restrictedRoot = p.IsRoot
	}

	ctx, cancel := context.WithTimeout(r.Context(), fileStatTimeout)
	defer cancel()

	// Resolve the project root once so each path costs a single EvalSymlinks.
	// Check empty BEFORE EvalSymlinks: EvalSymlinks("") returns (".", nil)
	// and would bind resolution to the process CWD.
	if rootPath == "" {
		httputil.WriteJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		// Unresolvable root collapses to {exists:false}, matching the contract.
		httputil.WriteJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}

	results := make(map[string]existsEntry, len(req.Paths))
	for _, rel := range req.Paths {
		if err := ctx.Err(); err != nil {
			// Timeout: return what we have; the frontend treats unknowns as
			// "no button", preserving the text-only fallback.
			break
		}
		entry := statRelWithRoot(rootResolved, rel)
		// Existence-probe parity with HandleFileGet's restricted-root gates
		// (#831): without it the batch API could enumerate /tmp/<other-uid>/*
		// via {Exists, Size, Mime} even though GET refuses the bytes.
		// Re-Lstat so owner/mode are evaluated on the same inode.
		if entry.Exists && restrictedRoot {
			if abs, rerr := resolveProjectFileWithRoot(rootResolved, rel); rerr == nil {
				// Credential-name parity with servePreview/serveDownload so a root
				// spanning sibling projects cannot leak {exists,size,mime} for
				// credential files. Scan the resolved `abs`, not the raw `rel`: a
				// symlink `pub -> secrets` would otherwise evade the segment match.
				if isSensitiveDownloadPath(workspaceScanPath(rootResolved, abs)) {
					entry = existsEntry{Exists: false}
				} else if isPublicTmpDeniedName(abs) {
					// Deny sensitive names (sockets / pid / core dumps) so IPC
					// endpoints are not enumerable even when world-readable (#1330).
					entry = existsEntry{Exists: false}
				} else if info, lerr := os.Lstat(abs); lerr == nil &&
					(isPublicTmpForeignPrivate(info) || isPublicTmpIrregularType(info)) {
					// Non-regular-type parity with HandleFileGet (#1688).
					entry = existsEntry{Exists: false}
				}
			}
		}
		results[rel] = entry
	}

	httputil.WriteJSON(w, map[string]any{"results": results})
}

// statRelWithRoot stats a single project-relative path and returns the
// metadata the dashboard needs to decide preview vs download. Errors collapse
// to {exists:false}. Callers pass an already-resolved root so batch sites
// don't pay N × EvalSymlinks.
func statRelWithRoot(rootResolved, rel string) existsEntry {
	resolved, err := resolveProjectFileWithRoot(rootResolved, rel)
	if err != nil {
		return existsEntry{Exists: false}
	}
	// Lstat (not Stat): a symlink installed after EvalSymlinks (TOCTOU) is
	// reported as not-existing rather than followed.
	info, err := os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return existsEntry{Exists: false}
	}
	if info.IsDir() {
		return existsEntry{Exists: true, IsDir: true, Size: info.Size()}
	}

	// Skip the open+read sniff when the extension alone resolves the MIME;
	// batches are dominated by .go/.py/.md/.json, so this saves one
	// open+512B-read per path and relieves fileStatTimeout on NFS/HDD.
	mime := ""
	if info.Size() == 0 {
		mime = "text/plain"
	} else if m, ok := mimeFromExtOnly(resolved); ok {
		mime = m
	} else {
		// Peek the first 512 bytes for MIME detection; not cached across calls
		// since mtime changes would stale it and the cost is the open, not the read.
		f, openErr := os.Open(resolved)
		if openErr == nil {
			head := make([]byte, 512)
			n, _ := io.ReadFull(f, head)
			f.Close()
			mime = detectMime(resolved, head[:n])
		}
	}
	return existsEntry{Exists: true, Size: info.Size(), Mime: mime}
}

// mimeFromExtOnly returns the extension-derived MIME when the path alone
// unambiguously resolves it, so statRelWithRoot's batch path can skip the
// sniff. Returns ok only when the sniff would yield the same answer: .svg is
// pinned regardless of bytes, and previewableByExt entries are what the sniff
// path itself falls back to. Empty extensions and binary formats fall through.
func mimeFromExtOnly(resolved string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext == ".svg" {
		return "image/svg+xml", true
	}
	if ext == "" {
		// Extensionless files need basename lookup; defer to detectMime.
		return "", false
	}
	if v, ok := previewableByExt[ext]; ok {
		return v, true
	}
	return "", false
}

// HandleFileGet serves GET /api/projects/file?project=X&path=Y&mode=preview|raw|render|download.
//   - preview: JSON {content, truncated, size, mime}; text only, capped to
//     maxPreviewBytes, invalid UTF-8 replaced with U+FFFD.
//   - raw: inline stream with Content-Type=mime, capped to maxRawBytes.
//   - render: octet-stream attachment for the dashboard's blob-URL iframe (see serveRender).
//   - download: octet-stream attachment; http.ServeContent handles Range.
//
// ETag is sha256(size||mtime||FileETagSalt)[:12] in all modes; 304 on If-None-Match.
func (h *Handlers) HandleFileGet(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	project := r.URL.Query().Get("project")
	path := r.URL.Query().Get("path")
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "preview"
	}
	if mode != "preview" && mode != "raw" && mode != "download" && mode != "render" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid mode"})
		return
	}
	if project == "" || path == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project and path are required"})
		return
	}
	// __public_tmp__ pseudo-project (see publicTmpProject godoc): resolve
	// against /tmp; the traversal / symlink / credential guards still apply.
	rootPath := ""
	// restrictedRoot marks a root the dashboard is NOT unconditionally cleared
	// to read: __public_tmp__ (/tmp) and the include_root whole-workspace
	// project. Both get the foreign-private / denied-name / irregular-type
	// gates and the audit log below; a registered subdirectory project is
	// readable by definition.
	restrictedRoot := false
	if project == publicTmpProject && h.publicTmpEnabled {
		rootPath = publicTmpRoot
		restrictedRoot = true
	} else {
		// Same trust-boundary gate as HandleFilesExists.
		if err := validateProjectName(project); err != nil {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid project name"})
			return
		}

		p := h.projectMgr.Get(project)
		if p == nil {
			httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		rootPath = p.Path
		restrictedRoot = p.IsRoot
	}

	resolved, err := resolveProjectFile(rootPath, path)
	if err != nil {
		// Missing vs outside-workspace collapse to 404 so probing yields the
		// same signal. Real IO errors (EACCES, EIO, EMFILE, …) are surfaced as
		// a Warn so ops can tell "operator typo" from "filesystem degraded";
		// gated on not-ErrNotExist AND not-a-path-shape rejection so crafted
		// paths cannot flood logs. Path itself is not logged (#651).
		if !errors.Is(err, fs.ErrNotExist) && !isClientPathRejection(err) {
			slog.Warn("project files: resolveProjectFile IO failure",
				"err", err,
				"project", project)
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Lstat instead of Stat: resolveProjectFile returned a symlink-free path,
	// so a symlink here means an attacker swapped it in during the TOCTOU
	// window. Reject as 404 to match the not-found / escape contract.
	info, err := os.Lstat(resolved)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Restricted roots (publicTmpProject, include_root): refuse owner-private
	// files owned by another UID (#831). Linux DAC checks the naozhi process,
	// not the dashboard caller, so a foreign 0600 file would otherwise flow
	// through. Same-UID and world/group-readable files stay accessible.
	if restrictedRoot && isPublicTmpForeignPrivate(info) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Name-based deny-list (#1330): even world/group-readable files such as
	// ssh-agent's 0o777 socket, core dumps and PID files must never be served.
	if restrictedRoot && isPublicTmpDeniedName(resolved) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Defence-in-depth type gate (#1688): refuse any non-regular file, e.g. a
	// world-readable custom-named IPC socket that passes the two gates above.
	if restrictedRoot && isPublicTmpIrregularType(info) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Audit trail for restricted roots (#1678): one structured log line per
	// served file so an operator who later shares the host can reconstruct
	// who read what. RemoteAddr, not request headers, to avoid echoing
	// attacker-controlled values into the log.
	if restrictedRoot {
		slog.Info("restricted_root file access",
			"project", project,
			"path", osutil.SanitizeForLog(resolved, 512),
			"mode", mode,
			"remote_addr", r.RemoteAddr)
	}

	// Defence-in-depth re-check that resolved still sits under the root: a
	// concurrent rename(2) between EvalSymlinks and Lstat could move the
	// containing dir outside the workspace while the inode-stable Lstat still
	// succeeds. A few extra syscalls, well below the body IO cost.
	rootResolved, rrErr := filepath.EvalSymlinks(rootPath)
	if rrErr != nil {
		// Surface IO failures (EACCES, EIO, …) as a Warn so ops can see a
		// degraded filesystem; fs.ErrNotExist is the legitimate "rootPath
		// just deleted" race and stays silent. Response is 404 either way —
		// leaking the errno would expose host filesystem state.
		if !errors.Is(rrErr, fs.ErrNotExist) {
			slog.Warn("project files: rootPath EvalSymlinks IO failure",
				"err", rrErr,
				"project", project,
				"path", osutil.SanitizeForLog(path, 256))
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if resolved != rootResolved &&
		!strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// Open ONCE with O_NOFOLLOW and plumb the fd into the serve* helpers
	// (#655): a per-helper os.Open after the Lstat guard would let a racing
	// attacker swap the file in between. O_NOFOLLOW closes the symlink-swap
	// leg atomically; the fstat IsRegular re-check closes the non-regular leg.
	// A same-workspace regular-file swap is unavoidable without openat2.
	f, err := OpenWorkspaceFile(resolved)
	if err != nil {
		// O_NOFOLLOW returns ELOOP on a final-component symlink; collapse to
		// 404 so probing cannot distinguish "missing" from "swapped".
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("project files: OpenWorkspaceFile IO failure",
				"err", err,
				"project", project)
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	// fstat the fd so size/mtime/mode describe the SAME inode the helpers
	// read; a swap to a dir/socket/fifo between Lstat and Open is rejected here.
	finfo, ferr := f.Stat()
	if ferr != nil || finfo.IsDir() || !finfo.Mode().IsRegular() {
		_ = f.Close()
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	info = finfo
	defer f.Close()

	// ETag = sha256(size | mtime-millis | FileETagSalt)[:12]. Millisecond
	// precision matches buildAttachmentETagSeed so both endpoints shed the
	// same timing-oracle bits; the per-process salt stops an If-None-Match
	// probe from recovering (size, mtime) (#418). strconv into a stack buffer
	// avoids fmt.Sprintf's reflection path.
	var etagBuf [80]byte
	etagSeed := strconv.AppendInt(etagBuf[:0], info.Size(), 10)
	etagSeed = append(etagSeed, '|')
	etagSeed = strconv.AppendInt(etagSeed, info.ModTime().UnixMilli(), 10)
	etagSeed = append(etagSeed, '|')
	etagSeed = append(etagSeed, FileETagSalt...)
	etagSum := sha256.Sum256(etagSeed)
	etag := `"` + hex.EncodeToString(etagSum[:12]) + `"`
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
		h.servePreview(w, f, rootResolved, resolved, info)
	case "raw":
		h.serveRaw(w, r, f, rootResolved, resolved, info)
	case "render":
		h.serveRender(w, r, f, rootResolved, resolved, info)
	case "download":
		h.serveDownload(w, r, f, rootResolved, resolved, info)
	}
}

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

	// Deliberately NOT text/html: octet-stream + attachment makes a direct
	// navigation download (Firefox ignores CSP sandbox there), while the
	// dashboard fetch() still gets the bytes for a client-side blob: URL.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", resolved))
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

// sensitiveDownloadNames lists exact filenames that commonly contain
// credentials; compared case-insensitively so ".ENV" doesn't slip through.
var sensitiveDownloadNames = map[string]struct{}{
	".env":             {},
	".env.local":       {},
	".env.dev":         {},
	".env.development": {},
	".env.prod":        {},
	".env.production":  {},
	".env.staging":     {},
	".env.test":        {},
	".netrc":           {},
	".npmrc":           {},
	".pypirc":          {},
	".dockercfg":       {},
	// SSH keys / authorized_keys carry no extension, so the ext list misses them.
	"id_rsa":          {},
	"id_dsa":          {},
	"id_ecdsa":        {},
	"id_ed25519":      {},
	"authorized_keys": {},
	"credentials":     {}, // ~/.aws/credentials, docker credentials helpers, etc.
	// Cloud-native credential filenames whose .json / .yaml extensions are too
	// broad for the extension allowlist; matched by full filename.
	"service-account.json":                 {},
	"serviceaccount.json":                  {},
	"service_account.json":                 {},
	"secrets.yaml":                         {},
	"secrets.yml":                          {},
	"secrets.json":                         {},
	"secret.yaml":                          {},
	"secret.yml":                           {},
	"gcp-key.json":                         {},
	"gcp_key.json":                         {},
	"gcloud-key.json":                      {},
	"firebase-adminsdk.json":               {},
	"application_default_credentials.json": {},
	"kubeconfig":                           {}, // legacy short name, also picked up via path
	// Ops-conventional credential files (Rails database.yml, DSN bundles,
	// api-keys.*); exact matches so "data.yml" / "config.yml" still preview.
	"database.yml":     {},
	"database.yaml":    {},
	"credentials.yml":  {},
	"credentials.yaml": {},
	"credentials.json": {},
	"api-keys.json":    {},
	"api-keys.yml":     {},
	"api-keys.yaml":    {},
	"api_keys.json":    {},
	"api_keys.yml":     {},
	"api_keys.yaml":    {},
	"rds.yml":          {},
	"rds.yaml":         {},
	"pg.yml":           {},
	"pg.yaml":          {},
	"mysql.yml":        {},
	"mysql.yaml":       {},
}

// sensitiveBaseSuffixes lists suffixes that identify backups / archives of
// credential files (".env.bak", ".env.old") so the exact-match table need not
// grow combinatorially.
var sensitiveBaseSuffixes = []string{
	".env.backup",
	".env.bak",
	".env.old",
	".env.orig",
	".env.save",
}

// sensitiveNameSubstrings is a defence-in-depth scan layered on the
// exact-name / extension / suffix rules (#1680): Claude routinely writes
// ad-hoc credential dumps with non-canonical names (`db-password.txt`,
// `aws_credentials.txt`, `api_token.log`). Matched case-insensitively as a
// basename substring. Kept deliberately narrow — no bare "key" token, since
// *.key is handled by sensitiveDownloadExts and "key" would block
// "keyboard.go" / "monkey.png".
var sensitiveNameSubstrings = []string{
	"password",
	"passwd",
	"secret",
	"credential", // matches credential / credentials, hyphen/underscore forms
	"token",
	"apikey",
	"api-key",
	"api_key",
	"private-key",
	"private_key",
	"privatekey",
}

// sensitiveDownloadExts lists extensions that strongly imply key material.
var sensitiveDownloadExts = map[string]struct{}{
	".key": {},
	".pem": {},
	".p12": {},
	".pfx": {},
	".crt": {}, // certs are usually fine, but combined with adjacent .key files
	".p8":  {}, // Apple/AWS/JWT private keys
}

// sensitivePathSegments lists directory names that, anywhere in the path,
// mark the whole subtree as credential-bearing, so `secrets/db.yaml` or
// `.ssh/known_hosts` cannot be exfiltrated on the strength of an innocent
// basename. Matched case-insensitively against every segment by
// isSensitiveDownloadPath; isSensitiveDownloadName keeps the basename contract.
var sensitivePathSegments = map[string]struct{}{
	".ssh":         {},
	".aws":         {},
	".gnupg":       {},
	".gpg":         {},
	".kube":        {},
	".docker":      {},
	".gcloud":      {},
	".azure":       {},
	"secrets":      {},
	"credentials":  {},
	"private-keys": {},
}

// isSensitiveDownloadPath reports whether any segment of relPath looks
// credential-bearing (sensitivePathSegments) or its basename does
// (isSensitiveDownloadName). Both `/` and the OS separator are honoured.
func isSensitiveDownloadPath(relPath string) bool {
	if relPath == "" {
		return false
	}
	// Split on both separators so a Windows-style path cannot bypass the scan.
	norm := strings.ReplaceAll(relPath, "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		low := strings.ToLower(seg)
		if _, ok := sensitivePathSegments[low]; ok {
			return true
		}
	}
	return isSensitiveDownloadName(filepath.Base(relPath))
}

// isSensitiveDownloadName reports whether base names a well-known
// credential-bearing file by fixed name, dotenv rule, extension or suffix.
func isSensitiveDownloadName(base string) bool {
	low := strings.ToLower(base)
	if _, ok := sensitiveDownloadNames[low]; ok {
		return true
	}
	// One rule for every dotenv variant (.env, .env.local, .env.example, …);
	// templates routinely carry secrets that became real. `.env` must be
	// followed by end-of-string or `.` so `.envoy.yaml` keeps previewing
	// (pinned in TestIsSensitiveDownloadName_OpsConventional).
	if low == ".env" || strings.HasPrefix(low, ".env.") {
		return true
	}
	if ext := filepath.Ext(low); ext != "" {
		if _, ok := sensitiveDownloadExts[ext]; ok {
			return true
		}
	}
	// Suffix scan for ".env.backup" / ".env.bak" style archive names.
	for _, suffix := range sensitiveBaseSuffixes {
		if strings.HasSuffix(low, suffix) {
			return true
		}
	}
	// Defence-in-depth substring scan for ad-hoc credential dumps (#1680).
	for _, sub := range sensitiveNameSubstrings {
		if strings.Contains(low, sub) {
			return true
		}
	}
	return false
}
