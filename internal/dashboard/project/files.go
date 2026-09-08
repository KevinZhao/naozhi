package project

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
