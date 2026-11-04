package project

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
)

// existsReq is the batch payload for POST /api/projects/files/exists.
type existsReq struct {
	Project string   `json:"project"`
	Node    string   `json:"node,omitempty"`
	Paths   []string `json:"paths"`
}

// existsEntry is one batch result (node.FileStat, the shape a node's
// files_exists answers too).
type existsEntry = node.FileStat

// HandleFilesExists serves POST /api/projects/files/exists: batch-stat up to
// maxExistsPaths paths under the project workspace so the dashboard can decide
// whether a path mentioned in a bubble gets preview/download buttons. Paths
// that don't resolve or fall outside the workspace come back as {exists:false}.
// node=<id> asks that node about its project (files_remote.go).
func (h *Handlers) HandleFilesExists(w http.ResponseWriter, r *http.Request) {
	r = httputil.WithMaxBytes(w, r, maxExistsBody)
	var req existsReq
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		slog.Debug("files exists: decode failed", "err", err)
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !isRemoteNode(req.Node) && h.deps.ProjectMgr == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "projects not configured"})
		return
	}

	if req.Project == "" {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}
	// Same validateProjectName trust-boundary gate as every other
	// /api/projects path; a future log of the miss path must not become a
	// log-injection hole.
	if err := validateProjectName(req.Project); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid project name"})
		return
	}
	if len(req.Paths) > maxExistsPaths {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("too many paths (max %d)", maxExistsPaths)})
		return
	}
	if isRemoteNode(req.Node) {
		h.remoteExists(w, r, req)
		return
	}
	res := h.existsIn(r.Context(), req.Project, req.Paths)
	if res.Status != 0 {
		writeRefusal(w, &res.FileRefusal)
		return
	}
	httputil.WriteJSON(w, res)
}

// FilesExistFor answers a primary's files_exists RPC with the batch
// HandleFilesExists serves, under this node's projects and rules.
func (h *Handlers) FilesExistFor(p node.FilesExistsParams) node.FilesExists {
	if err := validateProjectName(p.Project); err != nil {
		return node.FilesExists{FileRefusal: fileRefusal{Status: http.StatusBadRequest, Error: "invalid project name"}}
	}
	if len(p.Paths) > maxExistsPaths {
		return node.FilesExists{FileRefusal: fileRefusal{Status: http.StatusBadRequest, Error: fmt.Sprintf("too many paths (max %d)", maxExistsPaths)}}
	}
	return h.existsIn(context.Background(), p.Project, p.Paths)
}

// existsIn stats paths under the named project.
func (h *Handlers) existsIn(ctx context.Context, project string, paths []string) node.FilesExists {
	results := make(map[string]existsEntry, len(paths))
	if h.deps.ProjectMgr == nil {
		return node.FilesExists{FileRefusal: fileRefusal{Status: http.StatusBadRequest, Error: "projects not configured"}}
	}
	if len(paths) == 0 {
		return node.FilesExists{Results: results}
	}
	p := h.deps.ProjectMgr.Get(project)
	if p == nil {
		return node.FilesExists{FileRefusal: fileRefusal{Status: http.StatusNotFound, Error: "project not found"}}
	}
	rootPath := p.Path
	// restrictedRoot mirrors HandleFileGet: include_root projects get the
	// foreign-private / denied-name / irregular-type gates and the
	// credential-name filter so batch-exists cannot enumerate what GET refuses.
	restrictedRoot := p.IsRoot

	ctx, cancel := context.WithTimeout(ctx, fileStatTimeout)
	defer cancel()

	// Resolve the project root once so each path costs a single EvalSymlinks.
	// Check empty BEFORE EvalSymlinks: EvalSymlinks("") returns (".", nil)
	// and would bind resolution to the process CWD.
	if rootPath == "" {
		return node.FilesExists{Results: results}
	}
	rootResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		// Unresolvable root collapses to {exists:false}, matching the contract.
		return node.FilesExists{Results: results}
	}

	for _, rel := range paths {
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
				// Credential-name parity with preview/download so a root
				// spanning sibling projects cannot leak {exists,size,mime} for
				// credential files. Scan the resolved `abs`, not the raw `rel`: a
				// symlink `pub -> secrets` would otherwise evade the segment match.
				if isSensitiveDownloadPath(workspaceScanPath(rootResolved, abs)) {
					entry = existsEntry{Exists: false}
				} else if isRestrictedDeniedName(abs) {
					// Deny sensitive names (sockets / pid / core dumps) so IPC
					// endpoints are not enumerable even when world-readable (#1330).
					entry = existsEntry{Exists: false}
				} else if info, lerr := os.Lstat(abs); lerr == nil &&
					(isRestrictedForeignPrivate(info) || isRestrictedIrregularType(info)) {
					// Non-regular-type parity with HandleFileGet (#1688).
					entry = existsEntry{Exists: false}
				}
			}
		}
		results[rel] = entry
	}
	return node.FilesExists{Results: results}
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
