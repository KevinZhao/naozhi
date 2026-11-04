// files_remote.go — the file API for a node=<id> request: the node's own
// files, never this host's (planner RFC §15.5 ①④). A reverse node serves
// them over the files_list / file_read / files_exists RPCs, a direct node
// over its own file API; either way the node applies its roots, allowed_root,
// sensitive names and preview cap, and this host re-applies the raw / render
// / download type gates and headers to the bytes it relays.
package project

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// remoteFileTimeout bounds one file call to a node (one segment of a
// streamed file).
const remoteFileTimeout = 30 * time.Second

// remoteFiles returns node nodeID's file browser, or writes the answer and
// returns false: 400 for an unknown node, 501 for one that cannot serve its
// files (a reverse node predating node.CapFiles).
func (h *Handlers) remoteFiles(w http.ResponseWriter, nodeID string) (node.FileBrowser, bool) {
	if h.deps.NodeAccess == nil {
		writeRefusal(w, refuse(http.StatusNotImplemented, errRemoteFilesUnsupported))
		return nil, false
	}
	nc, ok := h.deps.NodeAccess.LookupNode(w, nodeID)
	if !ok {
		return nil, false
	}
	fb, ok := nc.(node.FileBrowser)
	if !ok || !nc.Meta().HasCap(node.CapFiles) {
		writeRefusal(w, refuse(http.StatusNotImplemented, errRemoteFilesUnsupported))
		return nil, false
	}
	return fb, true
}

// remoteRootOf reads a remote request's root=session:<key> or project=<name>,
// validated here as well as on the node.
func remoteRootOf(q url.Values) (node.FileRoot, *fileRefusal) {
	if root := q.Get("root"); root != "" {
		key, ok := strings.CutPrefix(root, sessionRootPrefix)
		if !ok {
			return node.FileRoot{}, refuse(http.StatusBadRequest, "invalid root")
		}
		if err := sessionkey.ValidateSessionKey(key); err != nil {
			return node.FileRoot{}, refuse(http.StatusBadRequest, "invalid session key")
		}
		return node.FileRoot{Key: key}, nil
	}
	name := q.Get("project")
	if name == "" {
		return node.FileRoot{}, refuse(http.StatusBadRequest, "project is required")
	}
	if err := validateProjectName(name); err != nil {
		return node.FileRoot{}, refuse(http.StatusBadRequest, "invalid project name")
	}
	return node.FileRoot{Project: name}, nil
}

// remoteRefusal is the answer for a node's refusal: a client-shaped status
// passes with its message bounded, anything else is the node failing.
func remoteRefusal(rf node.FileRefusal) *fileRefusal {
	switch rf.Status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return refuse(rf.Status, osutil.SanitizeForLog(rf.Error, 200))
	}
	return refuse(http.StatusBadGateway, "upstream error")
}

// writeRemoteErr answers a node file call that failed.
func writeRemoteErr(w http.ResponseWriter, nodeID string, err error) {
	if errors.Is(err, node.ErrFilesUnsupported) {
		writeRefusal(w, refuse(http.StatusNotImplemented, errRemoteFilesUnsupported))
		return
	}
	slog.Warn("remote files failed", "node", nodeID, "err", err)
	writeRefusal(w, refuse(http.StatusBadGateway, "upstream error"))
}

// remoteList serves HandleFilesList for node nodeID.
func (h *Handlers) remoteList(w http.ResponseWriter, r *http.Request, nodeID string, showHidden bool) {
	fb, ok := h.remoteFiles(w, nodeID)
	if !ok {
		return
	}
	q := r.URL.Query()
	fr, rf := remoteRootOf(q)
	if rf != nil {
		writeRefusal(w, rf)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteFileTimeout)
	defer cancel()
	list, err := fb.ListFiles(ctx, node.FilesListParams{FileRoot: fr, Dir: q.Get("dir"), ShowHidden: showHidden})
	if err != nil {
		writeRemoteErr(w, nodeID, err)
		return
	}
	if list.Status != 0 {
		writeRefusal(w, remoteRefusal(list.FileRefusal))
		return
	}
	// The dashboard joins names onto its breadcrumb: a name that is not one
	// path segment is not a child.
	entries := make([]listEntry, 0, min(len(list.Entries), maxListEntries))
	for _, e := range list.Entries {
		if len(entries) == maxListEntries {
			list.Truncated = true
			break
		}
		if e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\\\x00") {
			continue
		}
		entries = append(entries, e)
	}
	list.Entries = entries
	httputil.WriteJSON(w, list)
}

// remoteFileGet serves HandleFileGet for node nodeID: the preview as the node
// answers it, the other modes streamed from its raw segments.
func (h *Handlers) remoteFileGet(w http.ResponseWriter, r *http.Request, nodeID, path, mode string) {
	fb, ok := h.remoteFiles(w, nodeID)
	if !ok {
		return
	}
	fr, rf := remoteRootOf(r.URL.Query())
	if rf == nil && path == "" {
		rf = refuse(http.StatusBadRequest, "path is required")
	}
	if rf != nil {
		writeRefusal(w, rf)
		return
	}
	if mode == "preview" {
		ctx, cancel := context.WithTimeout(r.Context(), remoteFileTimeout)
		defer cancel()
		rd, err := fb.ReadFile(ctx, node.FileReadParams{FileRoot: fr, Path: path, Mode: "preview"})
		if err != nil {
			writeRemoteErr(w, nodeID, err)
			return
		}
		if rd.Status != 0 {
			writeRefusal(w, remoteRefusal(rd.FileRefusal))
			return
		}
		if len(rd.Data) > maxPreviewBytes {
			rd.Data, rd.Truncated = rd.Data[:maxPreviewBytes], true
		}
		w.Header().Set("Cache-Control", "no-store")
		httputil.WriteJSON(w, previewJSON(rd))
		return
	}
	streamRemote(w, r, fb, nodeID, node.FileReadParams{FileRoot: fr, Path: path, Mode: "raw"}, mode)
}

// streamRemote relays a node's file in raw, render or download mode: the
// first segment fixes the size and, sniffed here, the MIME the mode's gates
// judge; later segments must describe the same file. A file that changes
// mid-stream ends the body short of Content-Length, which the browser reports
// as a failed load rather than a spliced file.
func streamRemote(w http.ResponseWriter, r *http.Request, fb node.FileBrowser, nodeID string, p node.FileReadParams, mode string) {
	read := func(offset int64) (node.FileRead, error) {
		ctx, cancel := context.WithTimeout(r.Context(), remoteFileTimeout)
		defer cancel()
		p.Offset, p.Limit = offset, node.MaxFileChunk
		return fb.ReadFile(ctx, p)
	}
	first, err := read(0)
	if err != nil {
		writeRemoteErr(w, nodeID, err)
		return
	}
	if first.Status != 0 {
		writeRefusal(w, remoteRefusal(first.FileRefusal))
		return
	}
	size := first.Size
	if size < 0 || first.Offset != 0 || int64(len(first.Data)) > size || len(first.Data) > node.MaxFileChunk {
		writeRemoteErr(w, nodeID, errors.New("malformed file segment"))
		return
	}
	mime := ""
	if mode != "download" {
		mime = detectMime(p.Path, first.Data[:min(512, len(first.Data))])
	}
	if !prepareStream(w, r, mode, p.Path, mime, size) {
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(first.Data); err != nil {
		return
	}
	for off := int64(len(first.Data)); off < size; {
		seg, err := read(off)
		if err == nil && (seg.Status != 0 || seg.Size != size || seg.MtimeMs != first.MtimeMs ||
			seg.Offset != off || len(seg.Data) == 0 || int64(len(seg.Data)) > size-off) {
			err = errors.New("file changed or segment malformed")
		}
		if err != nil {
			slog.Warn("remote file stream cut short", "node", nodeID, "offset", off, "size", size, "err", err)
			return
		}
		if _, err := w.Write(seg.Data); err != nil {
			return
		}
		off += int64(len(seg.Data))
	}
}

// remoteExists serves HandleFilesExists for req.Node.
func (h *Handlers) remoteExists(w http.ResponseWriter, r *http.Request, req existsReq) {
	fb, ok := h.remoteFiles(w, req.Node)
	if !ok {
		return
	}
	results := make(map[string]existsEntry, len(req.Paths))
	if len(req.Paths) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), remoteFileTimeout)
		defer cancel()
		res, err := fb.FilesExist(ctx, node.FilesExistsParams{Project: req.Project, Paths: req.Paths})
		if err != nil {
			writeRemoteErr(w, req.Node, err)
			return
		}
		if res.Status != 0 {
			writeRefusal(w, remoteRefusal(res.FileRefusal))
			return
		}
		// Only the paths asked about come back.
		for _, p := range req.Paths {
			if e, ok := res.Results[p]; ok {
				results[p] = e
			}
		}
	}
	httputil.WriteJSON(w, map[string]any{"results": results})
}
