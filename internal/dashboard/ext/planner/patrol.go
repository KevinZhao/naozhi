package planner

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/patrol"
)

// The project patrol's deep check on demand (planner RFC §15.23 ②, D142):
//
//	POST /api/planner/patrol/run  {"workspace": "<workspace ID, or a name
//	                              no other shares>"} — a deep check of that
//	                              workspace now, whether or not it is due
//	                              or changed since the last; 202 with the
//	                              run ID, the record lands in <state
//	                              root>/planner/patrol/deep/<workspace
//	                              ID>.json. 404 for a workspace this node
//	                              does not have, 409 while one runs or
//	                              where none can.
//
// It sits behind the /api/* auth like the rest, and is not among a reverse
// node's `api` routes (node.APIRoutes): a deep check reads a repository on
// the machine that has it, started from that machine's own page, as the
// scout's run is.

// maxPatrolBody caps the request's body: one short field.
const maxPatrolBody = 4 << 10

// maxWorkspaceRef bounds the workspace named (workspace.Registry's names
// are shorter).
const maxWorkspaceRef = 128

// SetPatrol wires the project patrol; nil (no Planner state root) answers
// the run as off.
func (h *Handler) SetPatrol(p *patrol.Patrol) {
	if h != nil {
		h.patrol = p
	}
}

// HandlePatrolRun starts a deep check of a workspace now.
func (h *Handler) HandlePatrolRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string `json:"workspace"`
	}
	dec := json.NewDecoder(httputil.WithMaxBytes(w, r, maxPatrolBody).Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Workspace == "" || len(req.Workspace) > maxWorkspaceRef {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "workspace 要写工作区的 ID 或名字"})
		return
	}
	// A nil patrol answers ErrOff.
	id, err := h.patrol.Trigger(req.Workspace)
	switch {
	case errors.Is(err, patrol.ErrNoWorkspace):
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "run": id})
	}
}
