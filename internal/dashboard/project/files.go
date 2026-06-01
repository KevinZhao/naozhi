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

// FileETagSalt is a per-process random byte string mixed into the ETag
// hash for HandleFileGet. R214-SEC-4 (issue #418): the original
// sha256(size||mtime)[:8] form was theoretically probe-able — an
// authenticated caller who could enumerate (size, mtime) candidates
// could submit each as If-None-Match against a known path and use the
// 304-vs-200 oracle to recover both attributes from the response. By
// mixing in a 32-byte process-random salt the attacker can no longer
// precompute candidate ETags; size+mtime are still implicitly committed
// (so cacheability holds within a process) but the wire-visible bytes
// no longer leak them across processes.
//
// The salt is regenerated on every process start, which means client
// caches are invalidated on naozhi restart. That's an acceptable cost:
// project files are private, max-age=60, and a restart is expected to
// trigger a re-fetch anyway.
//
// Initialised lazily inside the package so test binaries that never
// touch the file API don't pay the crypto/rand setup cost. crypto/rand
// failure at init time is treated as a hard fault — the binary refuses
// to start rather than serving probe-able ETags. ProcessFromCryptorand
// failures during normal operation are pathologically rare (<1 in
// millions of years on modern Linux) so a single Read at init is the
// right ergonomic.
var FileETagSalt = func() []byte {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand init failure is fatal: serving ETags without salt
		// would silently regress the security property. Panicking at
		// package init is consistent with how upload_store.go's Put
		// handles in-flight rand.Read failures (errUploadStoreFull),
		// only escalated because we cannot return an error from a
		// package-level var initialiser.
		panic("crypto/rand unavailable for FileETagSalt: " + err.Error())
	}
	return b[:]
}()

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

// publicTmpProject is a reserved pseudo-project name that maps onto /tmp,
// letting the dashboard preview/download chat-mentioned absolute paths under
// /tmp without first registering /tmp as a real project.
//
// Trade-off: any authenticated dashboard user can read non-credential files
// anywhere under /tmp, including artefacts other users / processes left
// behind. Acceptable for naozhi's single-operator dashboard model; not safe
// in multi-tenant deployments. Symlinks that resolve outside /tmp are still
// rejected by resolveProjectFileWithRoot's prefix check, and the credential
// allowlist (.env, id_rsa, *.pem, etc.) still applies, so a malicious file
// dropped under /tmp cannot exfiltrate /etc/passwd or the operator's
// keystore.
//
// The handler intercepts this name before the projectMgr lookup so a real
// project named "__public_tmp__" on disk cannot accidentally shadow it.
//
// R237-SEC-5 (#646): the interception is gated by Handlers.publicTmpEnabled
// — default false so multi-user deployments fall through to the normal
// "project not found" surface. Single-operator setups flip it on via
// `server.public_tmp_enabled: true` in config.yaml.
const (
	publicTmpProject = "__public_tmp__"
	publicTmpRoot    = "/tmp"
)

// processEUID is the effective UID of the naozhi process. Captured once at
// init so isPublicTmpForeignPrivate avoids a syscall per request. Geteuid()
// on Linux is implemented as a cached read of the kernel's saved
// task->cred->euid; calling it per-file is cheap, but caching keeps the
// hot path syscall-free and lets tests override the value.
var processEUID = uint32(os.Geteuid())

// isPublicTmpForeignPrivate enforces R245-SEC-7 (#831): when the dashboard
// previews a path under /tmp via the publicTmpProject pseudo-project, the
// caller is trusted as "an authenticated dashboard user" but NOT as a
// system-level user. /tmp on a multi-user host can hold mode-0600 files
// owned by other UIDs (e.g. another operator's editor swap, an SSH socket,
// a systemd-private cache) — naozhi runs with read access to those bytes
// because Linux DAC permissions only check the *running process*, not the
// dashboard caller. Reflecting them through the dashboard would let the
// dashboard user trivially read another OS user's private files.
//
// Policy: refuse files whose mode bits indicate "owner-private" (no group /
// world bits) AND whose owner UID differs from the naozhi process's EUID.
// World/group-readable files (mode & 0o044 != 0) stay accessible — those
// the kernel already considers safe to share. Files owned by the same UID
// as naozhi are also fine: the operator running naozhi already owns them.
//
// The mode/UID pair is read from the FileInfo we already Lstat'd, so this
// is a zero-syscall check on the hot path.
func isPublicTmpForeignPrivate(info os.FileInfo) bool {
	uid, ok := fileOwnerUID(info)
	if !ok {
		// Non-Unix or stub FileInfo (test fakes): we cannot read the
		// owner UID, so this security gate fails closed — treat the
		// file as foreign-private and refuse it (deny by default).
		// The production build is always Linux where ok is always true,
		// so this branch never fires in production. A unit test that
		// wants to exercise the deny path supplies a real *syscall.Stat_t
		// via os.Lstat on a fixture file.
		return true
	}
	if uid == processEUID {
		return false
	}
	// Owner-private == no group OR world read/exec/write bits.
	const groupOrWorld = 0o077
	return info.Mode().Perm()&groupOrWorld == 0
}

// publicTmpDeniedSuffixes lists filename suffixes that must never be
// served through the __public_tmp__ pseudo-project even when the file is
// world/group readable. R20260527122801-SEC-6 (#1330): /tmp routinely
// holds Unix-domain sockets (mode 0o777 srwx by default) for ssh-agent,
// gpg-agent, postgres/redis IPC, etc. Linux DAC permissions check the
// running process, so naozhi's UID can connect()/read() those sockets
// transparently; reflecting their bytes through the dashboard is
// catastrophic — an `ssh-agent.<pid>` socket payload is the operator's
// authentication state. We also block .pid files (PID disclosure helps
// kill/ptrace probes) and core/crash dumps (memory snapshots that may
// contain secrets the producing process never intended to expose).
//
// Suffix matching is intentional: filenames like `agent.<pid>.sock`
// or `ssh-XXXXXX/agent.<pid>` should still trip the ssh substring check.
// All comparisons run against the basename of the resolved path so a
// directory component called "ssh" does not trigger denial.
var publicTmpDeniedSuffixes = []string{
	".sock",
	".pid",
}

// publicTmpDeniedSubstrings catches names that don't end in a known
// suffix (e.g. `ssh-agent.<pid>`, `core.1234`, `crash.report`). The
// match is case-insensitive on the basename. See publicTmpDeniedSuffixes
// for the rationale.
var publicTmpDeniedSubstrings = []string{
	"ssh",
}

// publicTmpDeniedPrefixes catches dump/crash artefacts whose names start
// with a known marker followed by a pid/timestamp. Matched on the
// case-insensitive basename so `core.1234` and `crash.txt` both trip.
var publicTmpDeniedPrefixes = []string{
	"core.",
	"crash.",
}

// isPublicTmpDeniedName reports whether the basename of resolved should
// be refused by the __public_tmp__ pseudo-project regardless of the
// file's mode bits. See publicTmpDeniedSuffixes godoc.
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
	// R244-SEC-P2-2: .html / .htm intentionally NOT mapped. servePreview
	// blocks text/html via HasPrefix gate (see line ~1040) and serveRaw
	// has the symmetric block, but listing the extension here would let
	// mimeFromExtOnly's batch fast path return text/html without sniffing
	// — a future caller that bypasses servePreview's mime gate (e.g. a
	// new render handler) would inherit the trust silently. Falling
	// through to detectMime keeps the canonical decision in one place
	// (the same byte-sniff path serveRender already gates on).
	".css":        "text/css",
	".sh":         "application/x-sh",
	".bash":       "application/x-sh",
	".zsh":        "application/x-sh",
	".sql":        "text/x-sql",
	".dockerfile": "text/plain",
	// R225-SEC-5: .env intentionally NOT mapped to text/plain — environment
	// files commonly hold API keys / database URLs / OAuth secrets.
	// Authenticated dashboard users with workspace browse permission could
	// otherwise hit ?path=.env&mode=preview and have the contents echoed
	// straight back as JSON. Falling through to DetectContentType keeps
	// .env served as application/octet-stream so servePreview's MIME guard
	// rejects it (the file can still be downloaded explicitly via raw mode
	// when the operator really intends to read it).
	".gitignore":     "text/plain",
	".gitattributes": "text/plain",
	".makefile":      "text/x-makefile",
	".mk":            "text/x-makefile",
	".proto":         "text/x-protobuf",
	".graphql":       "text/plain",
	".gql":           "text/plain",
	// R230B-SEC-4 / R232-SEC-1 / R233-SEC-5: .conf / .cfg / .ini are
	// deliberately previewable. Authenticated dashboard users have full
	// read access to the workspace already (download mode + raw mode +
	// Read tool from inside the CLI), so refusing preview only inflates
	// click-through cost without raising the security bar. Operators
	// must not store unencrypted secrets under allowed_root — that's the
	// invariant; we do NOT lower it to "secrets are OK if you store
	// them in .conf". Naming-pattern blocking (secret*.conf,
	// credentials.cfg, …) belongs in sensitiveDownloadNames /
	// sensitiveDownloadExts, not here.
	".conf": "text/plain",
	".cfg":  "text/plain",
	".ini":  "text/plain",
}

// rawPreviewMimes identifies file types the browser can render inline via <img>
// or <iframe>. Any MIME prefix here is allowed through mode=raw without forcing
// a download.
//
// SVG is intentionally absent: serveRaw rejects "image/svg+xml" downstream
// (stored XSS via <script> in workspace SVGs), and listing it here would create
// dead "passes preview, fails serveRaw" branches that a future refactor could
// silently turn into an XSS regression. SVG previews flow through serveRender
// (blob URL path) only.
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
// isClientPathRejection reports whether err is one of the well-known
// "client supplied a malformed path" rejections from resolveProjectFile /
// resolveProjectFileWithRoot. The handler logs ONLY genuine filesystem
// failures (EACCES, EIO, EMFILE, …) so a probing client cannot flood the
// logs with crafted paths. Matched by exact error.Error() string against
// the literals returned in this package; a future refactor that introduces
// a sentinel can drop this helper without changing call sites.
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
// paths (e.g. HandleFilesExists, which does up to 100 stats per request)
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
	// R244-SEC-P2-2: pin .html / .htm to text/html here ONLY so serveRender
	// can route empty / short HTML (where http.DetectContentType returns
	// text/plain) through its dedicated handler. Critically the mapping is
	// NOT carried in previewableByExt, so mimeFromExtOnly's batch fast path
	// cannot short-circuit the byte-sniff: any HTML reaching servePreview /
	// serveRaw still goes through detectMime, where the text/html result
	// triggers the existing HTML-block gates. Defense-in-depth — even a
	// future caller that skipped the gates would not silently inherit a
	// text/html response without a sniff confirmation on non-empty files.
	if ext == ".html" || ext == ".htm" {
		if strings.HasPrefix(mime, "text/plain") || strings.HasPrefix(mime, "application/octet-stream") {
			return "text/html"
		}
		return mime
	}
	// Base name override for extensionless files like Dockerfile / Makefile.
	// R232-SEC-8: paths whose basename starts with a dot (e.g. ".makefile",
	// ".gitignore") have filepath.Ext == basename, so this branch was
	// unreachable for them — and the previous "."+base concatenation produced
	// "..makefile" which never matched previewableByExt. Look up by basename
	// directly when ext is non-empty but Base starts with a dot.
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

// sanitizeDownloadName strips control characters and path separators from the
// filename used in Content-Disposition. A raw filename can smuggle CR/LF into
// response headers (HTTP response splitting) or cause Windows to treat the
// download as a path reference. filepath.Base handles the path; we still need
// to scrub control bytes the base retains.
//
// R175-SEC-LOW: also drop C1 controls (U+0080-U+009F) and the bidi/LS/PS class
// (U+202A-U+202E, U+2066-U+2069, U+2028, U+2029) that survive `r < 0x20`. The
// Content-Disposition header is built via RFC 6266's `filename*=UTF-8”...`
// with percent-encoding for non-ASCII, so C1 bytes would be passed through in
// percent-encoded form — some older HTTP intermediaries still choke on them,
// and bidi overrides let an attacker-supplied filename render as `foo.exe`
// despite the real extension being `foo.txt` when the file preview UI echos
// back to the operator. Aligns with the osutil.IsLogInjectionRune policy in
// dashboard_cron.go.
func sanitizeDownloadName(p string) string {
	base := filepath.Base(p)
	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop C0 controls
		case osutil.IsLogInjectionRune(r):
			// drop C1 controls + bidi override / isolate + LS / PS — same
			// policy as dashboard_cron.go and sanitizeClientFilename.
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
func (h *Handlers) HandleFilesExists(w http.ResponseWriter, r *http.Request) {
	// S13: Rate-limit before any work to cap the cost a single authenticated
	// caller can impose. The endpoint fans out up to maxExistsPaths (100)
	// filesystem stats per request with a fileStatTimeout (2s) budget; without
	// this gate a post-auth attacker targeting deep NFS mounts, symlink loops,
	// or gigantic directory trees can tie up worker goroutines. Nil-guarded for
	// tests that build Handlers by hand via newProjectHandlersForTest;
	// wiring lives in server.New (see Handlers.filesExistsLimiter godoc).
	if h.filesExistsLimiter != nil && !h.filesExistsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "files/exists rate limit exceeded"})
		return
	}
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	r = withMaxBytes(w, r, maxExistsBody)
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
	// __public_tmp__ pseudo-project routes /tmp/... preview without a real
	// project registration. Skip validateProjectName + projectMgr.Get for
	// this reserved name and pin rootPath to /tmp; everything else flows
	// through the same resolveProjectFileWithRoot guard so symlink escape /
	// path-traversal / credential-name rejection still apply.
	rootPath := ""
	if req.Project == publicTmpProject && h.publicTmpEnabled {
		rootPath = publicTmpRoot
	} else {
		// R183-SEC-M2: every other /api/projects path gates on validateProjectName
		// before touching projectMgr; HandleFilesExists previously passed raw
		// req.Project straight into the map lookup. The miss path is currently
		// silent, but one future slog.Debug("project not found", ...) is enough
		// to open a log-injection hole. Enforce the trust-boundary policy up front.
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
	if rootPath == "" {
		httputil.WriteJSON(w, map[string]any{"results": map[string]existsEntry{}})
		return
	}
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		// Treat an unresolvable project root as "nothing exists" so the
		// frontend renders plain text fallback. Matching the existing
		// contract: errors collapse to {exists:false}.
		httputil.WriteJSON(w, map[string]any{"results": map[string]existsEntry{}})
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
		entry := statRelWithRoot(rootResolved, rel)
		// R245-SEC-7 (#831): existence-probe parity with HandleFileGet's
		// foreign-private gate. Without this the dashboard could
		// enumerate /tmp/<other-uid>/* via the batch-exists API even
		// though HandleFileGet refuses to serve the bytes — the
		// {Exists, Size, Mime} fields alone leak presence + first-512-byte
		// MIME signature. Re-Lstat the resolved entry so we evaluate
		// owner/mode on the same inode the existence reply describes.
		// Cost: at most one extra Lstat per /tmp probe; the production
		// path is the project-root case where this branch is skipped.
		if entry.Exists && req.Project == publicTmpProject {
			if abs, rerr := resolveProjectFileWithRoot(rootResolved, rel); rerr == nil {
				// R20260527122801-SEC-6 (#1330): also deny sensitive
				// names (sockets / pid / core dumps / ssh-agent
				// artefacts) so the existence-probe API cannot be
				// used to enumerate IPC endpoints under /tmp even
				// when their mode bits are world-readable.
				if isPublicTmpDeniedName(abs) {
					entry = existsEntry{Exists: false}
				} else if info, lerr := os.Lstat(abs); lerr == nil && isPublicTmpForeignPrivate(info) {
					entry = existsEntry{Exists: false}
				}
			}
		}
		results[rel] = entry
	}

	httputil.WriteJSON(w, map[string]any{"results": results})
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
	// R218B-SEC-2: Lstat (not Stat) so a symlink installed after
	// resolveProjectFileWithRoot's EvalSymlinks (TOCTOU window) is
	// reported as not-existing rather than silently followed. The
	// resolved path is post-EvalSymlinks; encountering a symlink here
	// means the entry was replaced between resolve and stat.
	info, err := os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return existsEntry{Exists: false}
	}
	if info.IsDir() {
		return existsEntry{Exists: true, IsDir: true, Size: info.Size()}
	}

	// RNEW-PERF-006: skip the open+read sniff when the extension alone already
	// resolves to a known MIME. 100-path batch dashboards (project file picker)
	// are dominated by .go/.py/.md/.json, all of which previewableByExt covers
	// — short-circuiting saves one open+close+512B-read per path and makes the
	// 2s fileStatTimeout much less pressurised on NFS/HDD. Extensions not in
	// the table (or the empty-extension "Dockerfile"-ish path) still fall back
	// to the sniff so binary detection and source-code-without-extension keep
	// working.
	mime := ""
	if info.Size() == 0 {
		mime = "text/plain"
	} else if m, ok := mimeFromExtOnly(resolved); ok {
		mime = m
	} else {
		// Peek the first 512 bytes for MIME detection. On small files this is
		// the entire content; reading it here avoids a second open in the
		// preview handler later. We intentionally do NOT cache this across
		// calls — mtime changes would stale the cache and the per-call cost is
		// dominated by the open, not the read.
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
// unambiguously resolves it — no sniff required. Used by statRelWithRoot's
// batch fast path to avoid an open+read on every .go / .py / .md / .json
// in a 100-path batch. Returns (mime, true) only when we're confident the
// sniff would yield the same answer:
//   - .svg is pinned to image/svg+xml regardless of sniff (XSS gate in
//     detectMime); safe to short-circuit.
//   - previewableByExt entries are authoritative text/source types; the
//     sniff path ultimately calls this same table after DetectContentType
//     returns text/plain or application/octet-stream.
//
// Anything else (empty extension, binary formats like .png/.pdf where
// DetectContentType is the authority) falls through to the sniff path.
func mimeFromExtOnly(resolved string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(resolved))
	if ext == ".svg" {
		return "image/svg+xml", true
	}
	if ext == "" {
		// Extensionless files (Dockerfile, Makefile, LICENSE) need basename
		// lookup; defer to detectMime which handles it correctly.
		return "", false
	}
	if v, ok := previewableByExt[ext]; ok {
		return v, true
	}
	return "", false
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
// ETag is sha256(size||mtime||FileETagSalt)[:12] in all modes. 304 on
// If-None-Match. The per-process salt prevents probe-based recovery of
// (size, mtime) — see FileETagSalt godoc.
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
	// __public_tmp__ pseudo-project: see publicTmpProject godoc. Resolve
	// against /tmp instead of looking up a real project, but keep the same
	// path-traversal / symlink-escape / credential-name guards downstream.
	rootPath := ""
	if project == publicTmpProject && h.publicTmpEnabled {
		rootPath = publicTmpRoot
	} else {
		// R183-SEC-M2: same trust-boundary gate as HandleFilesExists above.
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
	}

	resolved, err := resolveProjectFile(rootPath, path)
	if err != nil {
		// os.ErrNotExist (valid but missing) vs outside-workspace collapse to
		// 404 — an attacker probing paths gets the same signal either way.
		//
		// R242-SEC-15 (#651): symmetric to the second-EvalSymlinks branch
		// below — surface real IO errors (EACCES, EIO, EMFILE, …) as a
		// structured Warn so ops can distinguish "operator typo" from
		// "filesystem degraded / permissions broken on rootPath". The
		// most common non-IO failures from resolveProjectFile are the
		// hostile-path rejections ("path escapes workspace", "path must
		// be relative", "invalid path") whose strings are stable. We
		// gate the Warn on fs.ErrNotExist absence AND not-a-path-shape
		// rejection so a probing client can't flood logs by sending
		// crafted paths. Path is NOT logged on the noisy IO branch
		// because it's user-supplied and potentially attacker-shaped;
		// the project name is sanitised via validateProjectName above.
		if !errors.Is(err, fs.ErrNotExist) && !isClientPathRejection(err) {
			slog.Warn("project files: resolveProjectFile IO failure",
				"err", err,
				"project", project)
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// R218B-SEC-2: Lstat instead of Stat so a symlink installed after
	// resolveProjectFile's EvalSymlinks (TOCTOU window) is rejected here
	// rather than silently followed. resolveProjectFile already returned
	// a fully-resolved path with no symlinks; if `resolved` is now a
	// symlink, an attacker replaced a real file with one in the gap.
	// Reject as 404 to match the rest of the not-found / escape contract.
	info, err := os.Lstat(resolved)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// R245-SEC-7 (#831): publicTmpProject lets any authenticated dashboard
	// user resolve absolute /tmp paths. Without this gate, a foreign UID's
	// 0600 file (other operator's editor swap, systemd-private socket
	// payload, …) flowed straight through because Linux DAC checks the
	// naozhi *process* — which has read access — not the dashboard caller.
	// Refuse owner-private files owned by a different UID; the same-UID
	// case (operator's own /tmp output) and world/group-readable files
	// stay accessible. Only applies to the publicTmpProject path; real
	// projects are operator-registered roots whose contents the dashboard
	// is by definition cleared to read.
	if project == publicTmpProject && isPublicTmpForeignPrivate(info) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// R20260527122801-SEC-6 (#1330): name-based deny-list for the
	// __public_tmp__ pseudo-project. Even when the foreign-private gate
	// above lets a file through (because it's world/group-readable, e.g.
	// ssh-agent's 0o777 socket), some names must never be served — Unix
	// sockets reflect IPC payload, core dumps contain process memory,
	// PID files leak process structure. See publicTmpDeniedSuffixes.
	if project == publicTmpProject && isPublicTmpDeniedName(resolved) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// R230-SEC-5: defence-in-depth re-check that resolved still sits under
	// the project root. resolveProjectFile already verified this once, but a
	// concurrent rename(2) between EvalSymlinks (inside resolveProjectFile)
	// and Lstat above could move the file's containing dir to a path outside
	// the workspace; the inode-stable Lstat then succeeds on a path that no
	// longer satisfies the prefix invariant. Re-evaluate the project root
	// once more so symlink-free escapes are caught on the same axis as the
	// symlink check above. The added EvalSymlinks call is bounded by a few
	// syscalls, well below the IO cost of the file body that follows.
	rootResolved, rrErr := filepath.EvalSymlinks(rootPath)
	if rrErr != nil {
		// R242-SEC-15: surface IO failures (EACCES, EIO, EMFILE, …) as a
		// Warn so ops can investigate. Previously every EvalSymlinks
		// failure silently collapsed to 404 — fine for the "user typed a
		// missing path" branch but blinds us to the rarer "filesystem
		// degraded" / "permissions broken on rootPath" cases. fs.ErrNotExist
		// is the legitimate "rootPath was just deleted" race and stays
		// silent; everything else gets a single structured log line so a
		// future SRE can grep for cron job IDs whose rootPath flapped.
		// Response stays 404 in both branches — surfacing the underlying
		// errno to the client would leak host filesystem state.
		if !errors.Is(rrErr, fs.ErrNotExist) {
			slog.Warn("project files: rootPath EvalSymlinks IO failure",
				"err", rrErr,
				"project", project,
				"path", path)
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if resolved != rootResolved &&
		!strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}

	// R219-SEC-2 (#655) + R220-GO-2: open the file ONCE here, with O_NOFOLLOW,
	// and plumb the resulting *os.File into the serve* helpers. Previously
	// each helper (serveRender / servePreview / serveRaw / serveDownload) ran
	// its own os.Open(resolved) AFTER the Lstat-after-resolve guard above —
	// an attacker who could win a sub-millisecond race could swap the regular
	// file for an unrelated regular file (different inode, same path) between
	// Lstat and Open and have the swapped bytes streamed under the original
	// path's authorization. O_NOFOLLOW closes the symlink-swap leg of the
	// TOCTOU kernel-atomically; the fstat-IsRegular re-check below closes the
	// "swap to non-regular file" leg. The remaining same-regular-different-
	// inode swap is unavoidable without renameat2/openat2 but the impact
	// shrinks to "attacker swaps File A's bytes with File B's bytes within
	// the same workspace+sensitive-name guard set" — bounded by the
	// authorization gate the request already passed.
	f, err := OpenWorkspaceFile(resolved)
	if err != nil {
		// O_NOFOLLOW returns ELOOP on a final-component symlink. Collapse to
		// 404 so attacker probing cannot distinguish "missing" from "swapped
		// to symlink" — same contract as the Lstat-symlink branch above.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("project files: OpenWorkspaceFile IO failure",
				"err", err,
				"project", project)
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	// fstat the fd so size/mtime/regular-mode reflect the SAME inode the
	// helpers will read from. info from Lstat above is intentionally
	// discarded for the body — it described a name, not a fd. A swap to a
	// dir/socket/fifo between Lstat and Open is rejected here.
	finfo, ferr := f.Stat()
	if ferr != nil || finfo.IsDir() || !finfo.Mode().IsRegular() {
		_ = f.Close()
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	info = finfo
	defer f.Close()

	// ETag hashes (size, mtime-ns) so the header does not leak exact byte
	// count or nanosecond modification timestamp to authenticated clients.
	// Matches the attachment endpoint convention — see handleAttachment.
	//
	// R224-PERF-4: same strconv-into-stack-buffer trick as dashboard_send's
	// handleAttachment to skip fmt.Sprintf's reflection path.
	//
	// R214-SEC-4 (issue #418): mix in FileETagSalt (per-process random 32
	// bytes) so the ETag bytes cannot be precomputed from candidate
	// (size, mtime) tuples. Without the salt an authenticated caller could
	// probe for the file's exact size+mtime via an If-None-Match oracle —
	// the salt closes that channel without breaking same-process caching
	// (the salt stays constant across requests until restart). The hash
	// prefix is also widened from 8 to 12 bytes to match the 96-bit
	// strength established by R246-SEC-13 for handleAttachment.
	var etagBuf [80]byte
	etagSeed := strconv.AppendInt(etagBuf[:0], info.Size(), 10)
	etagSeed = append(etagSeed, '|')
	etagSeed = strconv.AppendInt(etagSeed, info.ModTime().UnixNano(), 10)
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
		h.servePreview(w, f, resolved, info)
	case "raw":
		h.serveRaw(w, r, f, resolved, info)
	case "render":
		h.serveRender(w, r, f, resolved, info)
	case "download":
		h.serveDownload(w, r, f, resolved, info)
	}
}

// serveRender streams the bytes of a workspace .html / .svg file so the
// dashboard can embed it as a **blob URL** inside a sandboxed iframe for
// visual review (coverage reports, Playwright trace, pytest-html, generated
// SVG diagrams, etc).
//
// Threat model & design: workspace files are untrusted — Claude CLI's Write
// tool can drop any <script>...</script> into a .html or a .svg at any time
// (SVG supports inline <script>, on* event handlers, and external <use>
// references just like HTML). Rendering that content same-origin to the
// dashboard is stored-XSS. Three specific browser behaviors make naïve
// approaches unsafe:
//
//  1. Firefox ignores the HTTP `Content-Security-Policy: sandbox` directive
//     on top-level navigation (see the preexisting comment in serveRaw).
//     Setting the header alone is not enough — a user pasting the render
//     URL into a new tab gets a same-origin document in Firefox.
//  2. X-Frame-Options + CSP frame-ancestors only cover iframe embedding,
//     not top-level navigation.
//  3. The iframe `sandbox=""` attribute DOES cover both cases — but only if
//     the document sourced into the iframe has an origin distinct from
//     the dashboard, OR if allow-same-origin is absent (which drops us into
//     an opaque origin regardless of URL).
//
// To make this robust across browsers this handler deliberately does NOT
// serve `Content-Type: text/html` or `image/svg+xml`. Instead it returns
// `application/octet-stream` + `Content-Disposition: attachment` so a direct
// URL navigation always downloads the file instead of rendering it. The
// dashboard JS fetches the bytes, wraps them in a Blob with the right
// effective type (text/html for HTML, image/svg+xml for SVG), and feeds the
// resulting blob: URL into a sandboxed iframe. Blob URLs carry an opaque
// origin — even if sandbox is stripped by a future refactor, the document
// cannot read dashboard cookies or same-origin fetch.
//
// MIME gating still happens server-side (reject non-allowlisted at the
// boundary instead of relying on the client) so bytes that would sniff as a
// different type can't flow through this route at all.
//
// Size cap mirrors serveRaw (maxRawBytes, 50 MB) so a pathologically large
// file doesn't wedge the dashboard tab allocating the Blob.
//
// Known limitation: relative-path resources (<img src="./foo.png">, external
// CSS, web fonts, SVG <use href="#sym">-into-other-files) inside the rendered
// document will fail because the blob URL has no base path and default-src is
// 'none'. This matches B1 scope — most report generators (`go tool cover
// -html`, Playwright trace, pytest-html) and most SVG diagrams emit self-
// contained single-file content and are unaffected. Relative-asset support is
// B2, gated on actual user demand.
func (h *Handlers) serveRender(w http.ResponseWriter, r *http.Request, f *os.File, resolved string, info os.FileInfo) {
	// R249-SEC-5: mirror servePreview / serveRaw / serveDownload — refuse
	// credential-bearing names even when the bytes happen to sniff as
	// text/html or image/svg+xml. Without this gate an attacker who can
	// drop or rename a .env / id_rsa / .npmrc with HTML-shaped contents
	// could read it through render mode despite the other three modes
	// blocking it. The sensitive list is full-path scanned so subtree
	// stashes like `secrets/db.yaml` or `.ssh/known_hosts` are caught.
	if isSensitiveDownloadPath(resolved) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "render blocked for sensitive file name"})
		return
	}
	if info.Size() > maxRawBytes {
		httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline render; use download mode"})
		return
	}

	// R219-SEC-2 (#655): fd opened once by HandleFileGet with O_NOFOLLOW and
	// plumbed in; no second os.Open here so an inode-swap between Lstat and
	// serve* cannot redirect the streamed bytes. Caller owns Close.

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

	// Deliberately NOT text/html. application/octet-stream + attachment
	// disposition ensures:
	//   (a) A direct URL navigation downloads rather than renders, neutering
	//       the Firefox-ignores-CSP-sandbox top-level-nav attack vector.
	//   (b) The dashboard fetch() path still receives the raw bytes and
	//       constructs a blob: URL client-side, where the iframe sandbox
	//       contract is reliable.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", resolved))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Belt-and-braces CSP: if a future change flips Content-Type back to
	// text/html, the sandbox keeps the document in an opaque origin so it
	// cannot reach dashboard cookies / DOM. Harmless on the octet-stream
	// path.
	//
	// script-src 'unsafe-inline' 'unsafe-eval' is intentional: workspace
	// HTML routinely embeds MathJax / KaTeX / Mermaid / chart libs as
	// <script>...</script>, and MathJax in particular needs eval. Origin
	// isolation comes from the blob URL (opaque) + iframe sandbox (no
	// allow-same-origin), NOT from CSP — script execution here cannot read
	// dashboard cookies regardless. Removing 'unsafe-inline' would silently
	// break inline math rendering with no security benefit.
	//
	// R245-SEC-10: img-src deliberately restricted to data: blob: only.
	// 'self' here is the document's blob: URL origin (opaque), so the
	// keyword adds no real loading capability for the rendered HTML —
	// but a workspace document loaded via a future code-path that flipped
	// back to a same-origin document URL would be able to phone home with
	// `<img src=/api/sessions/state>` etc., probing dashboard endpoints
	// for side-channel signal (load timing → state-change tracking) even
	// though credentials are stripped by COEP/CORP. Tighten to data: blob:
	// so cron transcript / report HTML can still inline base64 PNGs and
	// load chart-lib-generated blob URLs, while every other origin is
	// refused at the CSP layer regardless of how the document is served.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; sandbox allow-scripts; script-src 'unsafe-inline' 'unsafe-eval' blob: data:; style-src 'unsafe-inline'; img-src data: blob:; font-src data:")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Workspace bytes must not sit in shared proxy caches under no-auth
	// deployments. HandleFileGet already wrote Cache-Control: private,
	// max-age=60 + ETag before dispatching; a no-store response with a
	// validator is semantically inconsistent, so we drop the ETag too.
	// Blob-URL consumers on the client re-fetch cheaply; no 304 needed.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("ETag")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// servePreview returns the first ~maxPreviewBytes of a workspace file as JSON
// so the dashboard drawer can render it with syntax highlighting. The `content`
// field flows through httputil.WriteJSON with SetEscapeHTML disabled, so the CLIENT
// MUST assign it via `textContent` or pass it through DOMPurify/a whitelist
// renderer before `innerHTML`. File contents are user-writable — Claude CLI
// tools create/edit files arbitrarily — so raw innerHTML would be a stored-XSS
// sink. dashboard.js currently uses `<pre><code>esc(content)</code></pre>`
// with esc() HTML-escaping the payload, satisfying this contract. R71-SEC-L1.
func (h *Handlers) servePreview(w http.ResponseWriter, f *os.File, resolved string, info os.FileInfo) {
	// Mirror the serveDownload guard: a file like .netrc / .npmrc / id_rsa
	// has a text MIME and would otherwise have its raw contents echoed in
	// the JSON `content` field. The download path's credential allowlist
	// must apply here too, otherwise an attacker can preview-read what
	// they cannot download.
	// R247-SEC-10: now scans every path segment so subtree-style stashes
	// like `secrets/db.yaml` or `.ssh/known_hosts` no longer slip past the
	// basename-only check.
	if isSensitiveDownloadPath(resolved) {
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

	// R219-SEC-2 (#655): fd plumbed in by HandleFileGet. No second os.Open;
	// caller owns Close.

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
	// R176-SEC-H3: text/html files MUST NOT flow through the preview JSON
	// content path. httputil.WriteJSON disables HTML escaping (SetEscapeHTML(false),
	// dashboard.go), so any <script> bytes in the workspace file land
	// verbatim inside the response JSON — the dashboard currently uses
	// `<pre><code>esc(content)</code></pre>` which is safe, but that is a
	// JS-side convention one regression away from stored XSS. serveRaw
	// already rejects text/html / image/svg+xml via explicit HasPrefix
	// guards (see below); mirror that contract here so the server-side
	// defense is symmetric across preview and raw modes. A Claude CLI
	// tool writing `<script>fetch('/api/sessions')</script>` to any
	// .html file in the workspace cannot reach the dashboard renderer.
	// HasPrefix covers detector outputs that append parameters like
	// "text/html; charset=utf-8".
	// R179-SEC-2: extend the guard to XML/XHTML MIMEs — an XHTML document
	// parsed by a browser executes <script>, so if the preview JSON's content
	// field ever becomes innerHTML (a single JS regression), stored XSS from
	// a workspace .xml is reachable. Mirror the serveRaw guard so preview and
	// raw are defense-symmetric.
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

	// Replace invalid UTF-8 so JSON encoding doesn't fail and <pre> doesn't
	// render as garbled bytes. A text file with a BOM or Latin-1 bytes would
	// otherwise abort the entire response.
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

func (h *Handlers) serveRaw(w http.ResponseWriter, r *http.Request, f *os.File, resolved string, info os.FileInfo) {
	// R246-SEC-2: enforce the same sensitive-name guard as servePreview /
	// serveDownload. A file like .env / id_rsa / .npmrc sniffs to text/plain
	// and would otherwise pass the isTextMime check below, exposing
	// credentials inline despite preview/download already refusing them.
	// R247-SEC-10: full-path scan so e.g. `.ssh/foo` is rejected even when
	// the basename is innocuous.
	if isSensitiveDownloadPath(resolved) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "preview blocked for sensitive file name"})
		return
	}
	if info.Size() > maxRawBytes {
		httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large for inline preview; use download mode"})
		return
	}

	// R219-SEC-2 (#655): fd plumbed in by HandleFileGet. No second os.Open;
	// caller owns Close.

	// Sniff MIME from file head so we don't hand the browser octet-stream for
	// images; http.ServeContent reads content-type from w.Header() if set.
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
	//
	// R179-SEC-2: application/xml and application/xhtml+xml encompass XHTML
	// documents that modern browsers parse with full DOM+script support when
	// served inline. A crafted .xml in the workspace with an XHTML namespace
	// + <script> block achieves same-origin script execution on top-level
	// navigation, bypassing the CSP sandbox (which only applies in iframe
	// embedding). Route these to the download guard like text/html and SVG.
	// text/xml is equivalent to application/xml for XHTML purposes.
	if strings.HasPrefix(mime, "text/html") || strings.HasPrefix(mime, "image/svg+xml") ||
		strings.HasPrefix(mime, "application/xhtml") ||
		strings.HasPrefix(mime, "application/xml") || strings.HasPrefix(mime, "text/xml") ||
		// R232-SEC-6: text/markdown does not get HTML-rendered by mainstream
		// browsers, but a UA that does (or a future MIME sniffer that maps
		// it to text/html) would face the same same-origin top-level
		// navigation risk as text/html / xhtml. Force the download guard
		// so the dashboard's preview button only ever streams markdown
		// through the sanitised renderer (servePreview / dashboard.js
		// renderMd) and never as a direct opaque inline doc.
		strings.HasPrefix(mime, "text/markdown") {
		httputil.WriteJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "inline preview disabled for this type; use download mode"})
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
		// R219-SEC-2 (#655): HandleFileGet now opens once and plumbs the fd
		// through; serveDownload no longer re-opens, so we hand off the same
		// *os.File directly. HandleFileGet's deferred Close stays the sole
		// owner — serveDownload only reads, never closes.
		h.serveDownload(w, r, f, resolved, info)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
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

func (h *Handlers) serveDownload(w http.ResponseWriter, r *http.Request, f *os.File, resolved string, info os.FileInfo) {
	// SEC-009: deny credential-bearing files even on the explicit download
	// path. servePreview already excludes .env via previewableByExt + the
	// MIME guard, but download had no equivalent stop, letting authenticated
	// users pull .env / .netrc / *.pem out of any workspace.
	// R247-SEC-10: full-path scan blocks `secrets/db.yaml`, `.ssh/foo` etc.
	if isSensitiveDownloadPath(resolved) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "file type not downloadable"})
		return
	}

	// R219-SEC-2 (#655): fd plumbed in by HandleFileGet (or relayed from
	// serveRaw's PDF branch). No second os.Open; ownership stays with the
	// caller's deferred Close.
	//
	// serveRaw's PDF hand-off may have already advanced the fd reading the
	// first 512 bytes for MIME sniffing; rewind so http.ServeContent streams
	// from offset 0.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "seek failed"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", resolved))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// Same rationale as serveRaw: workspace file bytes must not sit in
	// shared proxy caches under no-auth deployments. R71-SEC-L2.
	w.Header().Set("Cache-Control", "no-store")

	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// sensitiveDownloadNames lists exact filenames that commonly contain
// credentials and should never be served as a download. Compared
// case-insensitively so ".ENV" doesn't slip through on case-preserving FS.
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
	// SSH private keys and authorized_keys carry no extension by convention,
	// so the extension allowlist below cannot catch them.
	"id_rsa":          {},
	"id_dsa":          {},
	"id_ecdsa":        {},
	"id_ed25519":      {},
	"authorized_keys": {},
	"credentials":     {}, // ~/.aws/credentials, docker credentials helpers, etc.
	// Cloud-native credential filenames (GCP / Kubernetes / Firebase / generic
	// secrets) that show up in workspaces under allowed_root. The .json /
	// .yaml extensions are too broad for the extension allowlist (would block
	// legitimate config files), so match them here by full filename.
	// R232-SEC-4 + R230-SEC-? consolidated.
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
	// R233B-SEC-5: ops-conventional credential-laden filenames missed by the
	// .env / secret(s) anchors above. database.yml is Rails canonical; rds.yml
	// / pg.yml are common for PG/MySQL DSN bundles; credentials.yml +
	// credentials.yaml are Capistrano/Ansible style; api-keys.* covers the
	// ad-hoc convention. Listed as exact matches (not ext-only) so a
	// developer's legitimate "data.yml" / "config.yml" still preview/download.
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

// sensitiveBaseSuffixes lists filename suffixes that identify backups /
// archives of credential files (e.g. ".env.backup", ".env.bak", ".env.old").
// R233B-SEC-5: an attacker who can exfil "secrets.json" can equally exfil
// "secrets.json.bak"; suffix matching closes that obvious flank without
// growing the exact-match table to combinatorial size.
var sensitiveBaseSuffixes = []string{
	".env.backup",
	".env.bak",
	".env.old",
	".env.orig",
	".env.save",
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

// sensitivePathSegments lists directory names that — anywhere in the path —
// imply the entire subtree is credential-bearing. Basename-only matching let
// callers exfiltrate files like `secrets/db.yaml` or `.ssh/known_hosts`
// because the basename was an innocent `db.yaml` / `known_hosts`. Each entry
// is matched case-insensitively against any path segment.
//
// R247-SEC-10 [BREAKING-LOCAL]: callers used to pass filepath.Base(resolved)
// to isSensitiveDownloadName. The three production sites (servePreview,
// serveRaw, serveDownload) now pass the full resolved path to
// isSensitiveDownloadPath instead, which scans every segment with this
// allowlist *and* runs the legacy basename rule. Tests still call
// isSensitiveDownloadName directly so the basename contract is preserved.
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
// credential-bearing — either by the segment-level allowlist
// (sensitivePathSegments) or by the basename rule (isSensitiveDownloadName).
// relPath is interpreted as a filesystem path; both `/` and the OS separator
// are honoured so callers passing filepath.Clean output stay correct.
func isSensitiveDownloadPath(relPath string) bool {
	if relPath == "" {
		return false
	}
	// Split on both separators so a Windows-style path that leaks into the
	// resolver doesn't bypass the segment scan.
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

// isSensitiveDownloadName reports whether base (no path component) names a
// well-known credential-bearing file. Match both fixed names and risky
// extensions so workspace dotfiles like .env.production / id_rsa.pem cannot
// be exfiltrated through the download endpoint.
func isSensitiveDownloadName(base string) bool {
	low := strings.ToLower(base)
	if _, ok := sensitiveDownloadNames[low]; ok {
		return true
	}
	// R242-SEC-9: catch every dotenv variant in one rule rather than
	// growing sensitiveDownloadNames to N enumerated cases. .env.example
	// in particular previously slipped through the exact-match table —
	// detectMime would magic-byte sniff its `KEY=value` plaintext as
	// text/plain and the preview path would render it inline. .env files
	// shipped as templates routinely carry placeholder secrets that
	// accidentally become real ones (developers fill them in and forget
	// to delete the example). The match is `.env` followed by either
	// end-of-string or a `.` separator so legitimate names like
	// `.envoy.yaml` (envoy proxy config — pinned in
	// TestIsSensitiveDownloadName_OpsConventional allowed list) keep
	// previewing. Covers .env, .env.local, .env.production, .env.example,
	// .env.<anything>.
	if low == ".env" || strings.HasPrefix(low, ".env.") {
		return true
	}
	if ext := filepath.Ext(low); ext != "" {
		if _, ok := sensitiveDownloadExts[ext]; ok {
			return true
		}
	}
	// R233B-SEC-5: suffix scan catches ".env.backup" / ".env.bak" style
	// archive names that the exact-name + ext scans miss.
	for _, suffix := range sensitiveBaseSuffixes {
		if strings.HasSuffix(low, suffix) {
			return true
		}
	}
	return false
}
