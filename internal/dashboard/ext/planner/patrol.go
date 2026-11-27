package planner

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/front"
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
// node's `api` routes (node.IsAPIRoute): a deep check reads a repository on
// the machine that has it, started from that machine's own page, as the
// scout's run is.
//
// The user's say on what the patrol found (§15.23 ④), from the open items
// of the left column:
//
//	POST /api/planner/patrol/decide  {"ref": "<workspace ID>/<finding
//	                                 ID>", "choice": "do" | "skip" |
//	                                 "mute_kind"} — have the finding
//	                                 fixed, turn it down, or turn it down
//	                                 with its kind in its workspace;
//	                                 {"workspace": "<workspace ID>",
//	                                 "choice": "mute_ws"} — take the
//	                                 workspace out of the patrol. 200; 404
//	                                 for a finding not on record or a
//	                                 workspace this node does not have, 409
//	                                 for a choice the finding's state does
//	                                 not take or where the patrol's files
//	                                 do not read.
//
// A choice changes what the patrol does from then on (what it reports,
// where it looks, a task started), so it is taken as the ledger's edits
// are: only straight from loopback (403 otherwise), and the route is not
// among a reverse node's either. The cards in "通知" carry the same choices
// through POST /api/inbox/decide, as every card's answer goes.

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

// choiceMuteWS is the one choice about a workspace, not a finding.
const choiceMuteWS = "mute_ws"

// HandlePatrolDecide applies the user's choice about a finding, or about a
// workspace's patrol.
func (h *Handler) HandlePatrolDecide(w http.ResponseWriter, r *http.Request) {
	if !httputil.DirectLoopback(r) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "patrol_readonly_remote"})
		return
	}
	var req struct {
		Ref       string `json:"ref"`
		Workspace string `json:"workspace"`
		Choice    string `json:"choice"`
	}
	dec := json.NewDecoder(httputil.WithMaxBytes(w, r, maxPatrolBody).Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	var err error
	switch c := patrol.Choice(req.Choice); {
	case req.Choice == choiceMuteWS:
		if req.Ref != "" || !workspaceIDRE.MatchString(req.Workspace) {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "mute_ws 要写 workspace（工作区的 ID），不写 ref"})
			return
		}
		// A nil patrol answers ErrOff.
		err = h.patrol.MuteWorkspace(req.Workspace)
	case c == patrol.ChoiceDo || c == patrol.ChoiceSkip || c == patrol.ChoiceMuteKind:
		// The ref names the workspace and the finding in it: the finding
		// is looked up in that workspace's record, nowhere else.
		if _, _, ok := patrol.SplitRef(req.Ref); !ok || req.Workspace != "" {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "ref 要写 <工作区 ID>/<发现 ID>，不写 workspace"})
			return
		}
		err = h.patrol.Decide(req.Ref, c)
	default:
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "choice 要写 do、skip、mute_kind 或 mute_ws"})
		return
	}
	switch {
	case errors.Is(err, patrol.ErrNoFinding), errors.Is(err, patrol.ErrNoWorkspace):
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		// ErrChoice, ErrOff, and the patrol's files not reading.
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		httputil.WriteOK(w)
	}
}

// findingDetails are the patrol findings the open items point at, by ref:
// what the page shows under such an item.
func (h *Handler) findingDetails(items []front.Item) map[string]patrol.Detail {
	if h.patrol == nil {
		return nil
	}
	var refs []string
	for _, it := range items {
		if ref, ok := strings.CutPrefix(it.Source, front.PatrolSource); ok && it.From == front.FromPatrol {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return h.patrol.Details(refs)
}
