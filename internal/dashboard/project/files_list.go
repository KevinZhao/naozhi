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

// maxListEntries caps how many directory children a single GET
// /api/projects/files/list reply enumerates. A workspace dir can hold a
// node_modules / .git with 100k+ entries; without a cap the handler would
// Lstat every child (the per-entry Info() fan-out is the same DoS class as
// HandleFilesExists' 100-stat batch) and serialise a multi-MB JSON body.
// 2000 comfortably covers any human-navigable directory; beyond it the reply
// carries truncated:true so the UI can tell the user to narrow down.
const maxListEntries = 2000

// listEntry is one child in a directory listing. Absolute paths are NEVER
// serialised (parity with existsEntry) — the frontend only ever sees the
// child name and joins it onto the breadcrumb dir itself.
type listEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtime_ms"`
	// Symlink children are surfaced (so the operator knows they exist) but
	// flagged so the UI renders them non-navigable / non-downloadable — the
	// read + write paths both refuse to follow a final-component symlink, so
	// clicking one would only 404.
	Symlink bool `json:"symlink,omitempty"`
}

// GET /api/projects/files/list?project=X&node=&dir=Y
//
// Lists the immediate children of a directory under the project workspace.
// Powers the dashboard "文件" (workspace files) browser. Mirrors
// HandleFilesExists' security posture — rate-limit first, validate the
// project name, resolve under the project root with the symlink-escape /
// traversal guards — but enumerates a directory instead of stat-ing a fixed
// path list.
//
// dir is workspace-relative; "" or "." means the project root. Errors collapse
// to a single 404 (missing / outside-workspace / symlink / not-a-dir all look
// identical) so a probing client gets no oracle. Credential-named children are
// omitted entirely (never enumerated), matching the read/preview deny-list.
func (h *Handlers) HandleFilesList(w http.ResponseWriter, r *http.Request) {
	// Rate-limit before any filesystem work: directory enumeration fans out
	// one Info() per child (up to maxListEntries) — the same DoS class as
	// HandleFilesExists, so it shares that endpoint's limiter budget.
	if h.filesExistsLimiter != nil && !h.filesExistsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "files/list rate limit exceeded"})
		return
	}
	if h.projectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	// Remote-node workspaces are reached over a reverse-websocket and their
	// filesystem is not directly readable from this process. Reject rather
	// than silently listing the local dir under the same name.
	if node := r.URL.Query().Get("node"); node != "" && node != "local" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "file listing is not supported for remote nodes"})
		return
	}

	project := r.URL.Query().Get("project")
	dir := r.URL.Query().Get("dir")
	// show_hidden=1 surfaces dotfiles and noise directories (node_modules,
	// dist, …) that are hidden by default. The files-view UI keeps them out of
	// the way for everyday browsing but lets the operator opt in.
	showHidden := r.URL.Query().Get("show_hidden") == "1"
	if project == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}

	// __public_tmp__ is a read-of-arbitrary-/tmp pseudo-project gated behind
	// publicTmpEnabled. Browsing it as a tree would let an authenticated user
	// enumerate every /tmp entry, well beyond the "preview a chat-mentioned
	// path" intent the flag was scoped to. The workspace file browser is for
	// registered project roots only.
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
	// Empty BEFORE EvalSymlinks: EvalSymlinks("") returns (".", nil) on Linux,
	// which would bind resolution to the process CWD. R61-GO-1.
	if rootPath == "" {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "directory not found"})
		return
	}

	// Resolve the target directory. resolveProjectFileWithRoot rejects rel==""
	// ("path is required") and cleaned=="." ("path escapes workspace"), so the
	// "list the project root" case cannot flow through it — handle it directly.
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

	// Open the dir with O_NOFOLLOW so a final-component symlink swapped in
	// after resolution is refused (ELOOP → 404), and so we can ReadDir the
	// bounded child set straight off the fd. OpenWorkspaceFile is O_RDONLY,
	// which is fine for opening a directory on Linux.
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

	// ReadDir(maxListEntries+1) bounds the read at the syscall level — a
	// million-entry directory is never fully materialised. The +1 lets us
	// detect (and flag) truncation without reading the whole dir.
	dirents, rderr := f.ReadDir(maxListEntries + 1)
	if rderr != nil && !errors.Is(rderr, io.EOF) && !errors.Is(rderr, fs.ErrNotExist) {
		// ReadDir(n>0) returns io.EOF once fewer than n entries remain — and on
		// the FIRST call for an empty directory — so io.EOF is the normal
		// terminal signal, not a failure. Only a non-EOF error is genuine IO.
		slog.Warn("project files/list: readdir IO failure", "err", rderr, "project", project)
	}

	// truncated reflects the RAW child count (pre-filter): the directory held
	// more than maxListEntries entries, so this listing may be incomplete. It
	// is intentionally NOT the count of visible entries — computing that would
	// require reading (and filtering) the whole directory, defeating the cap
	// that bounds the ReadDir/Info fan-out. After hiding dotfiles/noise/
	// sensitive children the visible count can be well below maxListEntries
	// even when truncated is true; the flag means "narrow down to see the
	// rest", not "exactly maxListEntries shown".
	truncated := false
	if len(dirents) > maxListEntries {
		dirents = dirents[:maxListEntries]
		truncated = true
	}

	entries := make([]listEntry, 0, len(dirents))
	for _, de := range dirents {
		name := de.Name()
		// Omit credential-named children entirely — they must not even be
		// enumerable. Scans the full child path so a sensitive *segment*
		// (e.g. the dir is named ".ssh") is caught too.
		if isSensitiveDownloadPath(filepath.Join(dirResolved, name)) {
			continue
		}
		// Hide dotfiles and well-known noise directories unless the caller
		// opted in. Keeps the everyday listing focused on the operator's own
		// files instead of .git / node_modules / build output.
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

	// Dirs first, then names (case-insensitive) so the listing reads like a
	// file manager regardless of the OS readdir order. Pre-lowercase names once
	// (paired with their entry) so the comparator avoids two strings.ToLower
	// allocations per comparison — ~22k comparisons for a 2000-entry dir.
	sortEntries(entries)

	httputil.WriteJSON(w, map[string]any{
		"dir":       cleanDir,
		"entries":   entries,
		"truncated": truncated,
	})
}

// noiseBrowseDirs are directory names hidden from the files-view listing by
// default: build output, dependency trees, and tool caches that bury the
// operator's own files. Dotfiles are handled separately (name starts with ".")
// so this set only needs the non-dot offenders. show_hidden=1 bypasses both.
var noiseBrowseDirs = map[string]bool{
	"node_modules": true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"vendor":       true,
	"__pycache__":  true,
}

// isHiddenBrowseEntry reports whether a child name should be omitted from the
// default files-view listing: any dotfile (".git", ".naozhi", …) or a
// well-known noise directory. Pure name check — the caller has already applied
// the credential filter and decided whether show_hidden was requested.
func isHiddenBrowseEntry(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return noiseBrowseDirs[name]
}

// sortEntries orders entries dirs-first, then by case-insensitive name.
// It pre-computes the lowercased names once (paired with their entry so the
// pairing survives sort swaps) rather than calling strings.ToLower twice per
// comparison.
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
		return paired[i].lower < paired[j].lower
	})
	for i := range paired {
		entries[i] = paired[i].entry
	}
}
