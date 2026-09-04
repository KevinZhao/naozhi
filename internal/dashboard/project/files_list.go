package project

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// maxListEntries caps how many children one GET /api/projects/files/list
// reply enumerates: a node_modules / .git can hold 100k+ entries and the
// per-child Info() fan-out is the same DoS class as HandleFilesExists. Beyond
// the cap the reply carries truncated:true so the UI can ask to narrow down.
const maxListEntries = 2000

// listEntry is one child in a directory listing. Absolute paths are NEVER
// serialised; the frontend joins the name onto its breadcrumb dir.
type listEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtime_ms"`
	// Symlink children are surfaced but flagged so the UI renders them
	// non-navigable: the read and write paths refuse final-component symlinks.
	Symlink bool `json:"symlink,omitempty"`
}

// HandleFilesList serves GET /api/projects/files/list?project=X&node=&dir=Y:
// the immediate children of a workspace directory for the dashboard "文件"
// browser. Same security posture as HandleFilesExists (rate-limit, project
// name validation, symlink-escape / traversal guards). dir is
// workspace-relative; "" or "." is the root. All errors collapse to one 404
// so a probing client gets no oracle; credential-named children are never
// enumerated.
func (h *Handlers) HandleFilesList(w http.ResponseWriter, r *http.Request) {
	// Rate-limit before any filesystem work; shares HandleFilesExists' limiter
	// since per-child Info() fan-out is the same DoS class.
	if h.filesExistsLimiter != nil && !h.filesExistsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "files/list rate limit exceeded"})
		return
	}
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	// Remote-node filesystems are not readable from this process; reject rather
	// than silently listing the local dir under the same name.
	if node := r.URL.Query().Get("node"); node != "" && node != "local" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file listing is not supported for remote nodes"})
		return
	}

	project := r.URL.Query().Get("project")
	dir := r.URL.Query().Get("dir")
	// show_hidden=1 surfaces dotfiles and noise directories (node_modules,
	// dist, …) that the files view hides by default.
	showHidden := r.URL.Query().Get("show_hidden") == "1"
	if project == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}

	// __public_tmp__ is scoped to "preview a chat-mentioned path"; browsing it
	// as a tree would let a user enumerate all of /tmp. Registered roots only.
	if project == publicTmpProject {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "directory listing is not available for this scope"})
		return
	}
	if err := validateProjectName(project); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid project name"})
		return
	}

	p := h.projectMgr.Get(project)
	if p == nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	rootPath := p.Path
	// Empty BEFORE EvalSymlinks: EvalSymlinks("") returns (".", nil), which
	// would bind resolution to the process CWD.
	if rootPath == "" {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
		return
	}

	// resolveProjectFileWithRoot rejects rel=="" and cleaned==".", so the
	// "list the project root" case is handled directly.
	cleanDir := "."
	dirResolved := rootResolved
	if dir != "" && dir != "." {
		resolved, rerr := resolveProjectFileWithRoot(rootResolved, dir)
		if rerr != nil {
			if !errors.Is(rerr, fs.ErrNotExist) && !isClientPathRejection(rerr) {
				slog.Warn("project files/list: resolve IO failure", "err", rerr, "project", project)
			}
			httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
			return
		}
		dirResolved = resolved
		cleanDir = filepath.ToSlash(filepath.Clean(dir))
	}

	// Open with O_NOFOLLOW so a symlink swapped in after resolution is refused
	// (ELOOP → 404) and ReadDir runs straight off the fd.
	f, err := OpenWorkspaceFile(dirResolved)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("project files/list: open dir IO failure", "err", err, "project", project)
		}
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.IsDir() {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
		return
	}

	// ReadDir(maxListEntries+1) bounds the read at the syscall level; the +1
	// detects truncation without reading the whole dir.
	dirents, rderr := f.ReadDir(maxListEntries + 1)
	// truncated reflects the RAW child count (pre-filter): the directory held
	// more than maxListEntries, so the listing may be incomplete. Counting
	// visible entries would require reading the whole dir, defeating the cap;
	// the flag means "narrow down to see the rest".
	truncated := false
	if rderr != nil && !errors.Is(rderr, io.EOF) && !errors.Is(rderr, fs.ErrNotExist) {
		// ReadDir(n>0) returns io.EOF as the normal terminal signal (including
		// on an empty dir). A non-EOF error is genuine IO: keep what was read
		// but flag the listing as possibly incomplete.
		slog.Warn("project files/list: readdir IO failure", "err", rderr, "project", project)
		truncated = true
	}
	if len(dirents) > maxListEntries {
		dirents = dirents[:maxListEntries]
		truncated = true
	}

	entries := make([]listEntry, 0, len(dirents))
	for _, de := range dirents {
		name := de.Name()
		// Omit credential-named children entirely (never enumerable). Scans the
		// workspace-relative child path so a sensitive segment inside the workspace
		// is caught while a root itself named "secrets" is not (#2433).
		if isSensitiveDownloadPath(filepath.Join(workspaceScanPath(rootResolved, dirResolved), name)) {
			continue
		}
		// Hide dotfiles and noise directories unless the caller opted in.
		if !showHidden && isHiddenBrowseEntry(name) {
			continue
		}
		fi, ierr := de.Info()
		if ierr != nil {
			// Entry vanished between ReadDir and Info (race) — skip it.
			continue
		}
		mode := fi.Mode()
		if mode&os.ModeSymlink != 0 {
			entries = append(entries, listEntry{Name: name, Symlink: true})
			continue
		}
		// Skip irregular types (sockets, fifos, devices) — never legitimate
		// browsable content, parity with isPublicTmpIrregularType.
		if !mode.IsRegular() && !mode.IsDir() {
			continue
		}
		entries = append(entries, listEntry{
			Name:    name,
			IsDir:   mode.IsDir(),
			Size:    fi.Size(),
			MtimeMs: fi.ModTime().UnixMilli(),
		})
	}

	// Dirs first, then case-insensitive names, like a file manager regardless
	// of readdir order.
	sortEntries(entries)

	httputil.WriteJSON(w, map[string]any{
		"dir":       cleanDir,
		"entries":   entries,
		"truncated": truncated,
	})
}

// noiseBrowseDirs are non-dot directory names hidden from the files view by
// default (build output, dependency trees); show_hidden=1 bypasses.
var noiseBrowseDirs = map[string]bool{
	"node_modules": true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"vendor":       true,
	"__pycache__":  true,
}

// isHiddenBrowseEntry reports whether a child is omitted from the default
// listing: any dotfile or a well-known noise directory. Pure name check.
func isHiddenBrowseEntry(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return noiseBrowseDirs[name]
}

// sortEntries orders entries dirs-first, then case-insensitive name; names
// are lowercased once (paired with the entry) rather than twice per compare.
func sortEntries(entries []listEntry) {
	type entryWithLower struct {
		entry listEntry
		lower string
	}
	paired := make([]entryWithLower, len(entries))
	for i := range entries {
		paired[i] = entryWithLower{entry: entries[i], lower: strings.ToLower(entries[i].Name)}
	}
	sort.Slice(paired, func(i, j int) bool {
		if paired[i].entry.IsDir != paired[j].entry.IsDir {
			return paired[i].entry.IsDir
		}
		if paired[i].lower != paired[j].lower {
			return paired[i].lower < paired[j].lower
		}
		// Case-fold ties (Makefile / makefile): break on the original name so
		// the order is independent of ReadDir order (#2433).
		return paired[i].entry.Name < paired[j].entry.Name
	})
	for i := range paired {
		entries[i] = paired[i].entry
	}
}
