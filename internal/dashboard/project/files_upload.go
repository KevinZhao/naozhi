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
// generous: the primary use case is pushing build artefacts / archives / model
// files to a remote EC2 box through the dashboard instead of installing a
// second tool (vscode-web). The per-IP rate limiter (shared with files/exists,
// ≈10/min) plus this hard byte cap are the only DoS guards — there is no
// per-owner quota and no GC reaper, so uploaded files persist until the
// operator deletes them (documented trade-off, single-operator model).
const maxUploadFileBytes = 256 << 20

// uploadBodyOverhead is the multipart envelope slack added on top of the file
// byte cap when sizing http.MaxBytesReader: field names, boundaries, the
// Content-Disposition headers per part. 2 MiB is far more than a single-file
// form needs but keeps a clean 413 boundary well clear of legitimate uploads.
const uploadBodyOverhead = 2 << 20

// maxUploadMultipartFields caps non-file form values to stop a padded body
// from inflating the in-memory Value map. Mirrors the server-side
// rejectIfTooManyFields policy (maxMultipartFields=32).
const maxUploadMultipartFields = 32

// uploadMemThreshold is the in-memory spill threshold for ParseMultipartForm:
// parts larger than this stream to a temp file (which the handler RemoveAll's
// on return). 8 MiB keeps the common small-file path fully in memory while
// bounding peak RAM for the large-file path.
const uploadMemThreshold = 8 << 20

// uploadReadDeadline is the per-request body-read window for an upload,
// overriding the short global http.Server.ReadTimeout (15s) which would
// otherwise truncate a large upload over a normal uplink. 10 minutes lets a
// 256 MiB file land at ~3.4 Mbps sustained while still bounding a slow-loris
// body to a finite window. The deadline is absolute (not per-read), so it is
// a hard ceiling on how long one upload can pin a connection.
const uploadReadDeadline = 10 * time.Minute

// timeNow is the clock used for the per-request read deadline; a package var so
// tests can pin it. Defaults to time.Now.
var timeNow = time.Now

// writeOnlySensitiveNames are basenames that isSensitiveDownloadPath (a
// read/preview deny-list) does not cover but that must never be CREATED or
// OVERWRITTEN through the upload endpoint: shell rc / profile files (code
// execution on next login) and well-known control files. The read deny-list
// intentionally omits these because previewing a .bashrc is harmless; writing
// one is not. Matched case-insensitively against the leaf basename.
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
// target, mark the write as touching a control subtree the upload endpoint
// must refuse. ".git" protects repository integrity (a crafted hooks/post-
// checkout is code execution); ".naozhi" is naozhi's own state subtree
// (attachments, overrides) which user uploads must not poison; ".ssh" is
// already covered by the read deny-list but listed here for defence in depth.
var writeOnlySensitiveSegments = map[string]struct{}{
	".git":    {},
	".naozhi": {},
	".ssh":    {},
	".hg":     {},
	".svn":    {},
}

// isWriteBlockedPath reports whether relTarget (a workspace-relative,
// slash-or-OS-separated path) must be refused for WRITE. It layers the
// write-only rules on top of the shared read/preview credential deny-list, so
// every file the dashboard refuses to download is also refused as an upload
// target, plus the shell-rc / control-subtree names.
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

// validUploadLeaf validates that name is a single, safe path component to use
// as the destination filename. fh.Filename is fully attacker-controlled, so
// after stripping control/bidi runes via sanitizeDownloadName we assert it is
// exactly one component — no separators, no `.`/`..`, no NUL, within the ext4
// NAME_MAX. Returns the cleaned leaf and ok.
func validUploadLeaf(raw string) (string, bool) {
	// Reject separators / NUL on the RAW name first: sanitizeDownloadName runs
	// filepath.Base, which would silently turn "../../etc/passwd" into
	// "passwd" — accepting a traversal attempt as a valid leaf. Validate
	// before any Base collapse so such inputs are refused outright.
	if strings.ContainsAny(raw, "/\\") || strings.ContainsRune(raw, 0) {
		return "", false
	}
	leaf := sanitizeDownloadName(raw)
	// sanitizeDownloadName collapses empty / "." / ".." to the synthetic
	// "download"; a part whose name only differed by control bytes must not
	// silently land as a file literally called "download".
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

// POST /api/projects/files/upload (multipart/form-data)
//
// Uploads exactly one file into a directory under the project workspace. This
// is the only WRITE endpoint in the file API; CSRF is enforced upstream by
// RequireAuth (SameOriginOK runs automatically on POST — the handler MUST NOT
// re-check it and MUST NOT add a scheme-match gate, R247-SEC-1).
//
// Form fields:
//   - project (required): registered project name; the workspace root.
//   - dir (optional, default "."): workspace-relative TARGET directory, which
//     MUST already exist (the endpoint never MkdirAll's a client-named tree —
//     that would reopen the MkdirAll-follows-symlink class).
//   - file (exactly one part): the bytes; the part filename supplies the leaf.
//
// Query:
//   - overwrite=1: replace an existing file in place (O_TRUNC). Default refuses
//     with 409 (O_EXCL). O_NOFOLLOW is kept in both modes so a symlinked leaf
//     is never followed.
//   - node: remote nodes are unsupported → 400.
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

	// The global http.Server.ReadTimeout (15s, server.go) is a single absolute
	// budget for reading the whole body — far too short for a multi-hundred-MB
	// upload over a typical uplink, so without a per-handler override a large
	// (legitimate) upload would fail with a truncated-body parse error well
	// below the advertised cap. Extend the read deadline for THIS request only
	// to a generous-but-bounded window so big uploads succeed while slow-loris
	// body attacks stay bounded (the deadline is absolute, not a per-read
	// reset). SetReadDeadline returns ErrNotSupported if the ResponseWriter
	// doesn't implement it (e.g. some test recorders); ignore that — the
	// global timeout still applies as a safe floor.
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
	// ParseMultipartForm spills any part larger than uploadMemThreshold to a
	// temp file in os.TempDir(); with a 256 MiB cap the primary workload always
	// spills. Register cleanup before any further return so the temp file is
	// removed once the handler returns — io.Copy below has fully read it into
	// the destination by then. Mirrors the transcribe handler convention.
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

	// Validate the dir field shape with the same guard the read path uses for
	// relative paths. cleanRel reuses resolveProjectFileWithRoot's lexical
	// rules (no abs, no NUL, no `..`, length cap) without requiring the dir to
	// resolve yet — the EvalSymlinks happens below against the project root.
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
	// filesystem. Scans the full relative target so a sensitive segment in the
	// dir or a shell-rc leaf is caught.
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

	// Validate the PARENT directory (not the leaf — EvalSymlinks needs the
	// target to exist, and the leaf won't yet). resolveProjectFileWithRoot
	// EvalSymlinks the parent + prefix-checks it under rootResolved. The dir
	// must already exist as a real directory; we never MkdirAll a client tree.
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

	// Re-run the write deny-list on the RESOLVED target, not just the logical
	// one. cleanDir may EvalSymlinks to a control subtree the logical check
	// missed: a workspace symlink `docs -> .git` passes the `docs/hook` check
	// at relTarget above, but parentResolved is then `<root>/.git`, so writing
	// `<root>/.git/hook` would slip a git hook (code execution) past the
	// `.git`-segment guard. Deriving the workspace-relative path from the
	// resolved parent closes that symlinked-parent bypass.
	if rel, rerr := filepath.Rel(rootResolved, finalPath); rerr == nil && isWriteBlockedPath(filepath.ToSlash(rel)) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "this file name is not allowed"})
		return
	}

	// Create the leaf with O_NOFOLLOW (refuse symlinked leaf) + O_EXCL (refuse
	// silent overwrite) or O_TRUNC (explicit overwrite=1). This openat IS the
	// atomic security boundary for the leaf.
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

	// Copy with a hard ceiling (cap+1 to detect overflow); MaxBytesReader
	// already bounds the body, this is defence in depth against a lying
	// Content-Length.
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

	// fsync the file then the parent dir for crash durability, mirroring
	// osutil.WriteFileAtomic's ordering. A sync failure after a clean copy is
	// rare; surface it rather than reporting a success that may not survive.
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

	httputil.WriteJSON(w, map[string]any{
		"ok":   true,
		"path": filepath.ToSlash(path.Join(filepath.ToSlash(cleanDir), leaf)),
		"size": written,
	})
}

// lexicalRelOK reports whether rel is a safe workspace-relative path by the
// same lexical rules resolveProjectFileWithRoot applies before it touches the
// filesystem: non-empty, within the length cap, no NUL, not absolute, and no
// `..` escape after Clean. Used to validate the upload dir field shape without
// requiring the directory to resolve (that EvalSymlinks happens against the
// project root in the caller).
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
