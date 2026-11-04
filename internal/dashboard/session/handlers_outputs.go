package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// maxOutputsListed caps one GET /api/sessions/outputs response.
const maxOutputsListed = 500

// OutputSource is the outputs index (planner/activity.Recorder).
type OutputSource interface {
	Outputs(key string, limit int) []activity.Output
}

// HandleOutputs serves GET /api/sessions/outputs: the files sessions wrote and
// the PRs they opened, merged or linked to, newest first (planner RFC §10.2).
// key= lists that session's; without it every session's (the outputs
// library). A file's root + path go to /api/projects/file as they are; a file
// whose session is gone or now works elsewhere has no root (root=session:<key>
// would resolve to the wrong tree), only its dir + path. node=<id> with key=
// lists that node's session's outputs, as the node records them.
func (h *Handlers) HandleOutputs(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key != "" {
		if err := sessionkey.ValidateSessionKey(key); err != nil {
			http.Error(w, "invalid key parameter", http.StatusBadRequest)
			return
		}
	}
	if nodeID := r.URL.Query().Get("node"); nodeID != "" && nodeID != "local" {
		h.remoteOutputs(w, r, nodeID, key)
		return
	}
	httputil.WriteJSON(w, h.OutputsFor(key))
}

// outputsBody is the GET /api/sessions/outputs body.
type outputsBody struct {
	Outputs []activity.Output `json:"outputs"`
}

// OutputsFor is the outputs body for key ("" for every session's); a
// primary's fetch_outputs RPC answers with it too.
func (h *Handlers) OutputsFor(key string) any {
	outs := []activity.Output{}
	if h.deps.Outputs != nil {
		outs = h.deps.Outputs.Outputs(key, maxOutputsListed)
	}
	for i := range outs {
		if outs[i].Root != "" && !h.sessionWorksIn(outs[i].Key, outs[i].Dir) {
			outs[i].Root = ""
		}
	}
	return outputsBody{Outputs: outs}
}

// remoteOutputs lists a node session's outputs: 501 when the node cannot
// serve its files, since the rows exist to be previewed from there.
func (h *Handlers) remoteOutputs(w http.ResponseWriter, r *http.Request, nodeID, key string) {
	if key == "" {
		http.Error(w, "key is required for a remote node", http.StatusBadRequest)
		return
	}
	nc, ok := h.deps.NodeAccess.LookupNode(w, nodeID)
	if !ok {
		return
	}
	fb, ok := nc.(node.FileBrowser)
	if !ok || !nc.Meta().HasCap(node.CapFiles) {
		httputil.WriteJSONStatus(w, http.StatusNotImplemented, map[string]string{"error": "remote node does not support file access"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	raw, err := fb.FetchOutputs(ctx, key)
	if errors.Is(err, node.ErrFilesUnsupported) {
		httputil.WriteJSONStatus(w, http.StatusNotImplemented, map[string]string{"error": "remote node does not support file access"})
		return
	}
	var body outputsBody
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err != nil {
		slog.Warn("remote outputs failed", "node", nodeID, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	// Only the asked session's rows, bounded as a local answer.
	outs := make([]activity.Output, 0, min(len(body.Outputs), maxOutputsListed))
	for _, o := range body.Outputs {
		if o.Key == key && len(outs) < maxOutputsListed {
			outs = append(outs, o)
		}
	}
	httputil.WriteJSON(w, outputsBody{Outputs: outs})
}

// sessionWorksIn reports whether the local session key's workspace is dir.
func (h *Handlers) sessionWorksIn(key, dir string) bool {
	if h.deps.Router == nil || dir == "" {
		return false
	}
	sess := h.deps.Router.SessionFor(key)
	return sess != nil && sess.Workspace() == dir
}
