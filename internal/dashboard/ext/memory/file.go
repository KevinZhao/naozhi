package memory

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// /api/memories/file: one memory file of the 懂你 section, read, edited or
// deleted (planner RFC §15.15 ⑦). Only files that exist: nothing is created,
// renamed or moved (a new memory goes through the Planner's remember). A
// write is taken only from a request that reached this node's listener
// directly from loopback (directLoopback, D126 decision B); the routes the
// reverse-node `api` call reaches are the GETs only (node.APIRoutes).

// maxPutBody caps PUT's JSON body: the text at maxMemoryFileBytes, escaped.
const maxPutBody = 2 << 20

type fileResponse struct {
	Project      string `json:"project"`
	Name         string `json:"name"`
	Title        string `json:"title,omitempty"`
	Type         string `json:"type,omitempty"`
	Description  string `json:"description,omitempty"`
	Source       string `json:"source,omitempty"`
	RememberedAt string `json:"remembered_at,omitempty"`
	// Text is the whole file as it is on disk, for the editor; "" when
	// ReadOnly says why it cannot be edited here.
	Text string `json:"text"`
	// Body is the text after the frontmatter with control and bidi runes
	// dropped, for the rendered view.
	Body      string    `json:"body"`
	Truncated bool      `json:"truncated,omitempty"`
	ReadOnly  string    `json:"readonly,omitempty"`
	Version   string    `json:"version"`
	Modified  time.Time `json:"modified"`
	Size      int64     `json:"size"`
}

// version is what an edit or delete must name: the file as it was opened.
// mtime in nanoseconds and size, so a write by a CLI session in between is
// seen (a rename-replace that kept both is not).
func version(st os.FileInfo) string {
	return fmt.Sprintf("%d-%d", st.ModTime().UnixNano(), st.Size())
}

func writeErr(w http.ResponseWriter, status int, code string) {
	httputil.WriteJSONStatus(w, status, map[string]string{"error": code})
}

// writePathErr answers a path that did not resolve.
func writePathErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errGone):
		writeErr(w, http.StatusNotFound, "memory_gone")
	case errors.Is(err, errBadProject):
		writeErr(w, http.StatusBadRequest, "invalid_project")
	case errors.Is(err, errBadName):
		writeErr(w, http.StatusBadRequest, "invalid_name")
	case errors.Is(err, errOutside):
		writeErr(w, http.StatusBadRequest, "invalid_path")
	default:
		writeErr(w, http.StatusInternalServerError, "io")
	}
}

// readFile reads path into the answer for (project, name).
func readFile(project, name, path string) (fileResponse, error) {
	raw, truncated, err := readCappedMemoryFile(path, maxMemoryFileBytes)
	if err != nil {
		return fileResponse{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return fileResponse{}, err
	}
	resp := fileResponse{Project: project, Name: name, Truncated: truncated, Version: version(st), Modified: st.ModTime(), Size: st.Size()}
	switch {
	case truncated:
		resp.ReadOnly = roTooLarge
	case !utf8.Valid(raw):
		resp.ReadOnly = roNotUTF8
	default:
		resp.Text = string(raw)
	}
	meta, body := parseMemoryFrontmatter([]byte(strings.ToValidUTF8(string(raw), "�")))
	resp.Title = sanitizeWireText(meta.name)
	resp.Type = sanitizeWireText(meta.typ)
	resp.Description = sanitizeWireText(meta.description)
	resp.Source = sanitizeWireText(meta.source)
	resp.RememberedAt = sanitizeWireText(meta.rememberedAt)
	if name == indexName || project == "" {
		// The index and CLAUDE.md have no frontmatter to strip.
		body = strings.ToValidUTF8(string(raw), "�")
	}
	resp.Body = sanitizeWireText(body)
	return resp, nil
}

// HandleFile serves GET /api/memories/file?project=&name=.
func (h *Handler) HandleFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project, name := q.Get("project"), q.Get("name")
	var path string
	if project == "" && name == claudeMDName {
		if h.claudeMD == "" {
			writeErr(w, http.StatusNotFound, "memory_gone")
			return
		}
		st, err := os.Lstat(h.claudeMD)
		if err != nil || !st.Mode().IsRegular() {
			writeErr(w, http.StatusNotFound, "memory_gone")
			return
		}
		path = h.claudeMD
	} else {
		var err error
		if _, path, err = h.entryFile(project, name); err != nil {
			writePathErr(w, err)
			return
		}
	}
	resp, err := readFile(project, name, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "memory_gone")
			return
		}
		writeErr(w, http.StatusInternalServerError, "io")
		return
	}
	if project == "" {
		resp.ReadOnly = "claude_md"
		resp.Text = ""
	}
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, resp)
}

type putRequest struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Text    string `json:"text"`
	Version string `json:"version"`
}

// changed answers 409 with the file as it is now, which the page loads
// while keeping the user's draft.
func changed(w http.ResponseWriter, project, name, path string) {
	cur, err := readFile(project, name, path)
	if err != nil {
		writeErr(w, http.StatusConflict, "memory_changed")
		return
	}
	httputil.WriteJSONStatus(w, http.StatusConflict, struct {
		Error string `json:"error"`
		fileResponse
	}{"memory_changed", cur})
}

// HandlePut serves PUT /api/memories/file: the whole file replaced by text,
// when it is still the version the page opened. The gateway does not touch
// the frontmatter: a Planner-written entry keeps `source: naozhi-planner`,
// so the Planner's next remember of that name replaces it, the user's edit
// included; one whose source line the user removed can only be deleted here.
func (h *Handler) HandlePut(w http.ResponseWriter, r *http.Request) {
	if !directLoopback(r) {
		writeErr(w, http.StatusForbidden, "memory_readonly_remote")
		return
	}
	var req putRequest
	if err := httputil.DecodeJSONBody(httputil.WithMaxBytes(w, r, maxPutBody), &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "memory_too_large")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_body")
		return
	}
	if req.Project == "" {
		writeErr(w, http.StatusBadRequest, "invalid_project")
		return
	}
	if len(req.Text) > maxMemoryFileBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "memory_too_large")
		return
	}
	defer claudefs.LockMemory()()
	_, path, err := h.entryFile(req.Project, req.Name)
	if err != nil {
		writePathErr(w, err)
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		writePathErr(w, errGone)
		return
	}
	if req.Version == "" || req.Version != version(st) {
		changed(w, req.Project, req.Name, path)
		return
	}
	if st.Size() > maxMemoryFileBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "memory_too_large")
		return
	}
	if err := osutil.WriteFileAtomic(path, []byte(req.Text), st.Mode().Perm()); err != nil {
		slog.Warn("memory: edit failed", "project", req.Project, "name", req.Name, "err", err)
		writeErr(w, http.StatusInternalServerError, "io")
		return
	}
	st, err = os.Stat(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "io")
		return
	}
	slog.Info("memory: edited from the 懂你 section", "project", req.Project, "name", req.Name)
	httputil.WriteJSON(w, map[string]any{"version": version(st), "modified": st.ModTime(), "size": st.Size()})
}

// HandleDelete serves DELETE /api/memories/file?project=&name=&version=:
// the file goes, then the lines of its MEMORY.md linking "(<name>.md)";
// every other line stays as it was. MEMORY.md itself cannot be deleted.
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	if !directLoopback(r) {
		writeErr(w, http.StatusForbidden, "memory_readonly_remote")
		return
	}
	q := r.URL.Query()
	project, name, ver := q.Get("project"), q.Get("name"), q.Get("version")
	if name == indexName {
		writeErr(w, http.StatusBadRequest, "memory_index_undeletable")
		return
	}
	if project == "" {
		writeErr(w, http.StatusBadRequest, "invalid_project")
		return
	}
	defer claudefs.LockMemory()()
	dir, path, err := h.entryFile(project, name)
	if err != nil {
		writePathErr(w, err)
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		writePathErr(w, errGone)
		return
	}
	if ver == "" || ver != version(st) {
		changed(w, project, name, path)
		return
	}
	// The entry's own name in its (resolved) directory goes, not what a
	// symlink there points at: that is another listed entry or nothing.
	if err := os.Remove(filepath.Join(dir, name+".md")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writePathErr(w, errGone)
			return
		}
		slog.Warn("memory: delete failed", "project", project, "name", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "io")
		return
	}
	removed := 0
	if idx, err := h.resolveIn(dir, claudefs.MemoryIndexFile); err == nil {
		if removed, err = claudefs.EditMemoryIndex(idx, name, ""); err != nil {
			slog.Warn("memory: deleted, index not updated", "project", project, "name", name, "err", err)
			httputil.WriteJSON(w, map[string]any{"ok": true, "index_lines_removed": 0, "index_error": true})
			return
		}
	}
	slog.Info("memory: deleted from the 懂你 section", "project", project, "name", name, "index_lines", removed)
	httputil.WriteJSON(w, map[string]any{"ok": true, "index_lines_removed": removed})
}
