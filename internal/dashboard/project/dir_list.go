package project

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// workspaceProject is a reserved pseudo-project name that maps onto the
// dashboard's default workspace (session.cwd / router.DefaultWorkspace()).
// HandleDirList intercepts it before the projectMgr lookup so the directory
// browser can open the workspace root by name — without the operator first
// registering it as a real project — and then navigate down its subtree.
//
// Like publicTmpProject, the interception runs ahead of projectMgr.Get so a
// real on-disk project literally named "__workspace__" cannot shadow it. The
// workspace root is the same allowed_root the CLI session dispatcher already
// confines spawns to, so listing it grants no read surface the operator did
// not already have through the chat session itself.
const workspaceProject = "__workspace__"

// maxDirEntries caps the children returned for a single directory listing.
// A workspace directory with tens of thousands of entries (node_modules,
// a vendored monorepo, a log spool) would otherwise force the browser to
// render an unusable list AND let a single authenticated request fan out an
// unbounded number of Lstat calls. 2000 is far above any hand-navigable
// directory yet bounds the per-request syscall + JSON cost. When the cap
// trips we set Truncated so the UI can tell the operator the view is partial
// rather than silently implying the directory is small.
const maxDirEntries = 2000

// dirEntry is one child row in a directory listing.
type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	// Size is the regular-file byte count. Omitted for directories (a dir's
	// on-disk size is meaningless to the operator browsing it).
	Size int64 `json:"size,omitempty"`
}

// dirListResp is the GET /api/projects/dir response body.
type dirListResp struct {
	// Project echoes the resolved pseudo/real project name so the client can
	// keep issuing follow-up requests against the same root.
	Project string `json:"project"`
	// Path is the cleaned workspace-relative path that was listed ("" = root).
	Path string `json:"path"`
	// Parent is the workspace-relative path one level up, or "" when Path is
	// already the root. The client uses it to render the "↑ up" affordance
	// without re-deriving the parent (and possibly disagreeing with the
	// backend's idea of where the root boundary is).
	Parent string `json:"parent"`
	// AtRoot is true when Path is the project/workspace root — the client
	// hides the up affordance entirely rather than offering a no-op that
	// would (correctly) be rejected as an escape.
	AtRoot    bool       `json:"at_root"`
	Entries   []dirEntry `json:"entries"`
	Truncated bool       `json:"truncated,omitempty"`
}

// HandleDirList lists the immediate children (sub-directories and files) of a
// directory inside a project / the default workspace / __public_tmp__, for the
// dashboard's navigable folder browser.
//
//	GET /api/projects/dir?project=<name|__workspace__|__public_tmp__>&path=<rel>
//
// `path` is workspace-relative; empty means the root itself. The same
// path-traversal, symlink-escape, credential-name, and __public_tmp__ gates
// that protect HandleFileGet are reused here so the listing can never surface
// an entry the file endpoint would refuse to open. Directories are always
// listed; sensitive files (.env, id_rsa, *.pem, …) are omitted from the result
// so the browser never advertises a path the preview/download endpoints would
// reject anyway.
func (h *Handlers) HandleDirList(w http.ResponseWriter, r *http.Request) {
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	// Reuse the files-exists limiter: a directory listing does the same
	// class of unbounded FS fan-out (Lstat per child) as the batch existence
	// check, so it belongs to the same DoS budget. Nil-guarded for tests that
	// build Handlers by hand.
	if h.filesExistsLimiter != nil && !h.filesExistsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}

	projectName := r.URL.Query().Get("project")
	relPath := r.URL.Query().Get("path")
	if projectName == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}

	isPublicTmp := projectName == publicTmpProject && h.publicTmpEnabled
	rootPath, ok := h.dirListRoot(projectName)
	if !ok {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	// Resolve the target directory. Empty rel = the root itself; resolve it
	// directly (resolveProjectFileWithRoot rejects "" / "." as "path is
	// required" / "path escapes workspace"). A non-empty rel funnels through
	// the shared guard so traversal + symlink-escape are enforced identically
	// to HandleFileGet.
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		slog.Warn("project dir: root EvalSymlinks failed", "err", err, "project", projectName)
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
		return
	}

	cleanRel := ""
	resolved := rootResolved
	if relPath != "" {
		resolved, err = resolveProjectFileWithRoot(rootResolved, relPath)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !isClientPathRejection(err) {
				slog.Warn("project dir: resolve IO failure", "err", err, "project", projectName)
			}
			httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
			return
		}
		// Re-derive the workspace-relative form from the resolved absolute
		// path so the response (and the client's subsequent requests) speak a
		// canonical, symlink-free path rather than the raw client input.
		if rp, rerr := filepath.Rel(rootResolved, resolved); rerr == nil && rp != "." {
			cleanRel = filepath.ToSlash(rp)
		}
	}

	// Lstat (not Stat): a symlink swapped in after EvalSymlinks (TOCTOU) must
	// be rejected, not followed. The target must be a real directory.
	info, err := os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
		return
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		slog.Warn("project dir: ReadDir failed", "err", err, "project", projectName)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "cannot read directory"})
		return
	}

	resp := dirListResp{
		Project: projectName,
		Path:    cleanRel,
		AtRoot:  cleanRel == "",
		Entries: make([]dirEntry, 0, len(entries)),
	}
	if !resp.AtRoot {
		resp.Parent = parentRel(cleanRel)
	}

	// Bound the per-child Lstat fan-out by wall-clock, mirroring
	// HandleFilesExists (files.go): the maxDirEntries cap bounds syscall COUNT
	// but not per-syscall TIME, so a slow/hung backing store (NFS, FUSE,
	// stale automount) could otherwise pin a worker goroutine indefinitely.
	// On timeout we return the partial listing flagged Truncated, matching the
	// exists endpoint's "return what we have" contract.
	ctx, cancel := context.WithTimeout(r.Context(), fileStatTimeout)
	defer cancel()

	for _, de := range entries {
		if len(resp.Entries) >= maxDirEntries {
			resp.Truncated = true
			break
		}
		if ctx.Err() != nil {
			resp.Truncated = true
			break
		}
		name := de.Name()
		childAbs := filepath.Join(resolved, name)
		// Lstat the child so a symlink is classified by the link itself, not
		// its target. We never follow symlinks in the listing — a symlinked
		// directory could point outside the root, and following it to report
		// is_dir:true would invite the client to navigate across the boundary
		// (where resolveProjectFileWithRoot would then reject it anyway). Skip
		// symlinks entirely to keep the browsable tree inside the root.
		ci, cerr := os.Lstat(childAbs)
		if cerr != nil || ci.Mode()&os.ModeSymlink != 0 {
			continue
		}
		isDir := ci.IsDir()
		if !isDir && !ci.Mode().IsRegular() {
			// Sockets / FIFOs / devices are never browsable content.
			continue
		}
		// Credential / sensitive files are hidden from the listing so the
		// browser never advertises a path the file endpoints would refuse.
		// Directories are kept (a dir named "secrets" is navigable; its
		// sensitive *files* get filtered when that dir is listed).
		if !isDir && isSensitiveDownloadPath(childAbs) {
			continue
		}
		// __public_tmp__ applies the same per-entry gates HandleFilesExists
		// does before reporting a /tmp inode, so the listing and the open/exists
		// paths agree. The foreign-private + irregular-type gates run for
		// directories too (matching files.go): a mode-0700 dir owned by another
		// UID must not have its NAME enumerated via /tmp's world-listable
		// sticky bit — that would disclose other operators' private dir names
		// (systemd-private-*, .org.chromium.*, …) the exists endpoint hides.
		if isPublicTmp {
			if isPublicTmpForeignPrivate(ci) || isPublicTmpIrregularType(ci) || isPublicTmpDeniedName(childAbs) {
				continue
			}
		}
		entry := dirEntry{Name: name, IsDir: isDir}
		if !isDir {
			entry.Size = ci.Size()
		}
		resp.Entries = append(resp.Entries, entry)
	}

	// Directories first, then files; alphabetical within each group (case-
	// insensitive) so the browser presents a stable, conventional ordering.
	sort.SliceStable(resp.Entries, func(i, j int) bool {
		a, b := resp.Entries[i], resp.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	httputil.WriteJSON(w, resp)
}

// dirListRoot resolves a project query value to an absolute root directory.
// Returns ok=false when the name is unknown / not permitted. Mirrors
// HandleFileGet's project-name handling (workspace + publicTmp pseudo-projects
// intercepted before the real lookup, validateProjectName for everything
// else).
func (h *Handlers) dirListRoot(projectName string) (string, bool) {
	if projectName == workspaceProject {
		if h.router == nil {
			return "", false
		}
		ws := h.router.DefaultWorkspace()
		if ws == "" {
			return "", false
		}
		return ws, true
	}
	if projectName == publicTmpProject {
		if !h.publicTmpEnabled {
			return "", false
		}
		return publicTmpRoot, true
	}
	if err := validateProjectName(projectName); err != nil {
		return "", false
	}
	p := h.projectMgr.Get(projectName)
	if p == nil {
		return "", false
	}
	return p.Path, true
}

// parentRel returns the workspace-relative parent of a slash-separated
// workspace-relative path. The parent of a top-level entry ("foo") is the
// root, represented as "".
func parentRel(rel string) string {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return ""
	}
	return rel[:idx]
}
