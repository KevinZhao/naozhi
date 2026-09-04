package project

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// maxUploadFileBytes caps a single uploaded file. 256 MiB is deliberately
// generous (build artefacts / archives / model files pushed to a remote box).
// The per-IP rate limiter (shared with files/exists) plus this byte cap and
// the per-project uploadQuota are the only DoS guards; there is no GC reaper,
// so uploaded files persist until the operator deletes them.
const maxUploadFileBytes = 256 << 20

// uploadBodyOverhead is multipart envelope slack (field names, boundaries,
// per-part headers) added to the file cap when sizing http.MaxBytesReader.
const uploadBodyOverhead = 2 << 20

// maxUploadMultipartFields caps non-file form values so a padded body cannot
// inflate the in-memory Value map (mirrors httputil.MaxMultipartFields).
const maxUploadMultipartFields = 32

// uploadMemThreshold is ParseMultipartForm's in-memory spill threshold; larger
// parts stream to a temp file the handler RemoveAll's on return.
const uploadMemThreshold = 8 << 20

// uploadReadDeadline overrides the short global http.Server.ReadTimeout for an
// upload body: 10 minutes lets a 256 MiB file land at ~3.4 Mbps while still
// bounding a slow-loris body. The deadline is absolute, not per-read.
const uploadReadDeadline = 10 * time.Minute

// timeNow is the clock used for the per-request read deadline; a package var so
// tests can pin it. Defaults to time.Now.
var timeNow = time.Now

// writeOnlySensitiveNames are basenames the read/preview deny-list omits
// (previewing a .bashrc is harmless) but that must never be CREATED or
// OVERWRITTEN via upload: shell rc / profile files (code execution on next
// login) and well-known control files. Case-insensitive on the leaf basename.
var writeOnlySensitiveNames = map[string]struct{}{
	".bashrc":          {},
	".bash_profile":    {},
	".bash_login":      {},
	".bash_logout":     {},
	".profile":         {},
	".zshrc":           {},
	".zprofile":        {},
	".zshenv":          {},
	".zlogin":          {},
	".kshrc":           {},
	".cshrc":           {},
	".tcshrc":          {},
	".login":           {},
	".inputrc":         {},
	".gitconfig":       {},
	".git-credentials": {},
	"sudoers":          {},
	"crontab":          {},
	"authorized_keys2": {},
	"known_hosts":      {},
}

// writeOnlySensitiveSegments are path segments that, anywhere in the relative
// target, mark a control subtree uploads must not touch: ".git" (a crafted
// hook is code execution), ".naozhi" (naozhi's own state), VCS metadata, and
// ".ssh" for defence in depth.
var writeOnlySensitiveSegments = map[string]struct{}{
	".git":    {},
	".naozhi": {},
	".ssh":    {},
	".hg":     {},
	".svn":    {},
}

// isWriteBlockedPath reports whether relTarget (workspace-relative) must be
// refused for WRITE: the shared read/preview credential deny-list plus the
// shell-rc / control-subtree names.
func isWriteBlockedPath(relTarget string) bool {
	if isSensitiveDownloadPath(relTarget) {
		return true
	}
	norm := strings.ReplaceAll(relTarget, "\\", "/")
	for _, seg := range strings.Split(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		if _, ok := writeOnlySensitiveSegments[strings.ToLower(seg)]; ok {
			return true
		}
	}
	if _, ok := writeOnlySensitiveNames[strings.ToLower(path.Base(norm))]; ok {
		return true
	}
	return false
}

// validUploadLeaf validates that the attacker-controlled fh.Filename is a
// single safe path component after sanitizeDownloadName: no separators, no
// `.`/`..`, no NUL, within NAME_MAX. Returns the cleaned leaf and ok.
func validUploadLeaf(raw string) (string, bool) {
	// Reject separators / NUL on the RAW name first: sanitizeDownloadName's
	// filepath.Base would turn "../../etc/passwd" into an accepted "passwd".
	if strings.ContainsAny(raw, "/\\") || strings.ContainsRune(raw, 0) {
		return "", false
	}
	leaf := sanitizeDownloadName(raw)
	// sanitizeDownloadName collapses empty / "." / ".." to the synthetic
	// "download"; such a part must not silently land under that name.
	if leaf == "" || leaf == "." || leaf == ".." || leaf == "download" {
		return "", false
	}
	if leaf != filepath.Base(leaf) || leaf != path.Clean(leaf) {
		return "", false
	}
	if len(leaf) > 255 {
		return "", false
	}
	return leaf, true
}

// HandleFilesUpload serves POST /api/projects/files/upload (multipart/form-data):
// uploads exactly one file into an existing directory under the project
// workspace. This is the only WRITE endpoint in the file API; CSRF is enforced
// upstream by RequireAuth (SameOriginOK runs on POST — the handler MUST NOT
// re-check it or add a scheme-match gate).
//
// Form: project (required), dir (optional, default "."; must already exist —
// never MkdirAll'd), file (exactly one part; its filename supplies the leaf).
// Query: overwrite=1 replaces in place (O_TRUNC) instead of 409 (O_EXCL);
// O_NOFOLLOW applies in both modes. Remote nodes are unsupported → 400.
func (h *Handlers) HandleFilesUpload(w http.ResponseWriter, r *http.Request) {
	// Rate-limit first — before parsing the (potentially 256 MiB) body.
	if h.filesExistsLimiter != nil && !h.filesExistsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "files/upload rate limit exceeded"})
		return
	}
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}
	if node := r.URL.Query().Get("node"); node != "" && node != "local" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file upload is not supported for remote nodes"})
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "1"

	// The global http.Server.ReadTimeout (15s) is far too short for a
	// multi-hundred-MB upload, so extend the read deadline for THIS request to
	// a bounded window (absolute, so slow-loris stays bounded). ErrNotSupported
	// from test recorders is ignored; the global timeout remains a safe floor.
	_ = http.NewResponseController(w).SetReadDeadline(timeNow().Add(uploadReadDeadline))

	// Body cap BEFORE parse so an oversize body yields a clean 413 instead of
	// ParseMultipartForm's opaque "bad multipart form".
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadFileBytes+uploadBodyOverhead)
	if err := r.ParseMultipartForm(uploadMemThreshold); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large"})
			return
		}
		slog.Debug("files/upload: parse multipart failed", "err", err)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad multipart form"})
		return
	}
	// Parts above uploadMemThreshold spill to a temp file; register cleanup
	// before any further return so it is removed once the handler returns.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if r.MultipartForm != nil {
		nFields := 0
		for range r.MultipartForm.Value {
			nFields++
		}
		if nFields > maxUploadMultipartFields {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "too many form fields"})
			return
		}
	}

	project := r.FormValue("project")
	if project == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}
	if project == publicTmpProject {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "upload is not allowed for this scope"})
		return
	}
	if err := validateProjectName(project); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid project name"})
		return
	}

	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "exactly one file part is required"})
		return
	}
	fh := files[0]

	leaf, ok := validUploadLeaf(fh.Filename)
	if !ok {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid file name"})
		return
	}

	// Validate the dir field shape with the read path's lexical rules; the
	// EvalSymlinks happens below against the project root.
	cleanDir := "."
	dir := strings.TrimSpace(r.FormValue("dir"))
	if dir != "" && dir != "." {
		if !lexicalRelOK(dir) {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid directory"})
			return
		}
		cleanDir = filepath.Clean(dir)
	}

	// Refuse credential / control-file write targets BEFORE touching the
	// filesystem; scans the full relative target (dir segments + leaf).
	relTarget := path.Join(filepath.ToSlash(cleanDir), leaf)
	if isWriteBlockedPath(relTarget) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "this file name is not allowed"})
		return
	}

	p := h.projectMgr.Get(project)
	if p == nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	rootPath := p.Path
	if rootPath == "" {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "target directory not found"})
		return
	}

	// Validate the PARENT directory (the leaf does not exist yet, so it cannot
	// be EvalSymlinks'd). The dir must already be a real directory; we never
	// MkdirAll a client tree.
	parentResolved := rootResolved
	if cleanDir != "." {
		parentResolved, err = resolveProjectFileWithRoot(rootResolved, cleanDir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !isClientPathRejection(err) {
				slog.Warn("files/upload: parent resolve IO failure", "err", err, "project", project)
			}
			httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "target directory not found"})
			return
		}
	}
	if di, derr := os.Lstat(parentResolved); derr != nil || !di.IsDir() || di.Mode()&os.ModeSymlink != 0 {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "target directory not found"})
		return
	}

	finalPath := filepath.Join(parentResolved, leaf)

	// Re-run the write deny-list on the RESOLVED target: a workspace symlink
	// `docs -> .git` passes the logical `docs/hook` check but resolves to
	// `<root>/.git/hook`, which would slip a git hook past the `.git` guard.
	if rel, rerr := filepath.Rel(rootResolved, finalPath); rerr == nil && isWriteBlockedPath(filepath.ToSlash(rel)) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "this file name is not allowed"})
		return
	}

	// Per-project upload quota (#2311): reserve the declared part size BEFORE
	// writing a byte so one tenant cannot fill the shared disk. Clamp to
	// maxUploadFileBytes (the body is independently capped there, so a lying
	// header cannot under- or over-reserve past it). Released on any failure
	// path, reconciled against the true byte count on success; nil quota = no-op.
	reserveBytes := fh.Size
	if reserveBytes > maxUploadFileBytes {
		reserveBytes = maxUploadFileBytes
	}
	if !h.uploadQuota.reserve(project, reserveBytes) {
		httputil.WriteJSONStatus(w, http.StatusInsufficientStorage, map[string]string{"error": "project upload quota exceeded"})
		return
	}
	quotaCommitted := false
	defer func() {
		if !quotaCommitted {
			h.uploadQuota.release(project, reserveBytes)
		}
	}()

	// O_NOFOLLOW (refuse symlinked leaf) + O_EXCL (refuse silent overwrite) or
	// O_TRUNC (overwrite=1): this open IS the atomic security boundary for the leaf.
	dst, err := CreateWorkspaceFile(finalPath, overwrite)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": "file already exists"})
		case errors.Is(err, fs.ErrNotExist):
			httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "target directory not found"})
		default:
			// ELOOP (symlinked leaf) lands here; collapse to 409 so it is
			// indistinguishable from an ordinary conflict and leaks nothing.
			if isSymlinkLoopErr(err) {
				httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": "file already exists"})
				return
			}
			slog.Warn("files/upload: create IO failure", "err", err, "project", project,
				"target", osutil.SanitizeForLog(relTarget, 256))
			httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not create file"})
		}
		return
	}

	src, err := fh.Open()
	if err != nil {
		_ = dst.Close()
		if !overwrite {
			_ = os.Remove(finalPath)
		}
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "could not read uploaded file"})
		return
	}
	defer src.Close()

	// Hard ceiling (cap+1 detects overflow); defence in depth on top of
	// MaxBytesReader against a lying Content-Length.
	written, copyErr := io.Copy(dst, io.LimitReader(src, maxUploadFileBytes+1))
	if copyErr == nil && written > maxUploadFileBytes {
		copyErr = errors.New("file exceeds size limit")
	}
	if copyErr != nil {
		_ = dst.Close()
		if !overwrite {
			_ = os.Remove(finalPath)
		}
		if osutil.IsDiskFull(copyErr) {
			httputil.WriteJSONStatus(w, http.StatusInsufficientStorage, map[string]string{"error": "no space left on device"})
			return
		}
		if written > maxUploadFileBytes {
			httputil.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large"})
			return
		}
		slog.Warn("files/upload: copy IO failure", "err", copyErr, "project", project)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not write file"})
		return
	}

	// fsync file then parent dir for crash durability (osutil.WriteFileAtomic
	// ordering); surface a sync failure rather than report an unsafe success.
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		if !overwrite {
			_ = os.Remove(finalPath)
		}
		if osutil.IsDiskFull(err) {
			httputil.WriteJSONStatus(w, http.StatusInsufficientStorage, map[string]string{"error": "no space left on device"})
			return
		}
		slog.Warn("files/upload: fsync IO failure", "err", err, "project", project)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not persist file"})
		return
	}
	if err := dst.Close(); err != nil {
		if !overwrite {
			_ = os.Remove(finalPath)
		}
		slog.Warn("files/upload: close IO failure", "err", err, "project", project)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not persist file"})
		return
	}
	_ = osutil.SyncDir(parentResolved)

	// Commit the reservation now the file is durable, reconciling the reserved
	// estimate against the true byte count (release slack / charge extra).
	quotaCommitted = true
	if delta := reserveBytes - written; delta > 0 {
		h.uploadQuota.release(project, delta)
	} else if delta < 0 {
		// written exceeded the reservation (declared size under-reported);
		// charge the difference best-effort so the running total stays honest.
		_ = h.uploadQuota.reserve(project, -delta)
	}

	httputil.WriteJSON(w, map[string]any{
		"ok":   true,
		"path": filepath.ToSlash(path.Join(filepath.ToSlash(cleanDir), leaf)),
		"size": written,
	})
}

// lexicalRelOK reports whether rel passes the lexical rules
// resolveProjectFileWithRoot applies before touching the filesystem (length
// cap, no NUL, not absolute, no `..` escape after Clean), without requiring
// the directory to resolve yet.
func lexicalRelOK(rel string) bool {
	if rel == "" || len(rel) > maxExistsPathLen {
		return false
	}
	if strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
		return false
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
