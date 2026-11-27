package planner

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/patrol"
)

// The ledger of the user's lines of work on the Planner page (planner RFC
// §15.19, D134):
//
//	GET  /api/planner/fronts  the lines in progress, the latest updated
//	                          first, the open items (§15.23, D143) each
//	                          with the line it shows under, oldest first,
//	                          the project patrol's findings those items
//	                          point at (by ref: kind, size, evidence with
//	                          its links, impact, fix, pull request), and
//	                          whether this request may edit them
//	POST /api/planner/fronts  {"id", "name", "state", "next", "waiting",
//	                          "close"} — the user's own edit of one line
//	                          (a field left out stays, "" clears it);
//	                          {"item": {"text", "workspace", "front"}} —
//	                          an open item the user adds; {"item_done":
//	                          "<item id>"} — one the user strikes. One of
//	                          the three; only straight from loopback, 403
//	                          otherwise
//
// The read sits behind the /api/* auth like the rest of the page. Neither
// route is among a reverse node's `api` routes (node.IsAPIRoute): the ledger
// is shown and edited on the machine that keeps it.

// maxFrontBody caps an edit's body: a few short fields.
const maxFrontBody = 16 << 10

// SetFronts wires the ledger; nil (no Planner state root) lists none.
func (h *Handler) SetFronts(s *front.Store) { h.fronts = s }

type frontsResponse struct {
	Fronts []front.Front `json:"fronts"`
	Items  []front.Item  `json:"items"`
	// Findings are the patrol findings the items point at (source
	// "patrol:<ref>"), by ref; left out with none.
	Findings map[string]patrol.Detail `json:"findings,omitempty"`
	// Writable is whether an edit from this request would be taken
	// (httputil.DirectLoopback); the page offers none otherwise.
	Writable bool `json:"writable"`
}

// HandleFronts lists the lines of work in progress.
func (h *Handler) HandleFronts(w http.ResponseWriter, r *http.Request) {
	resp := frontsResponse{Fronts: []front.Front{}, Items: []front.Item{}, Writable: h.fronts != nil && httputil.DirectLoopback(r)}
	if h.fronts != nil {
		if open := h.fronts.Open(); open != nil {
			resp.Fronts = open
		}
		if items := h.fronts.Items(); items != nil {
			resp.Items = items
			resp.Findings = h.findingDetails(items)
		}
	}
	httputil.WriteJSON(w, resp)
}

// frontEdit is the user's edit of one line: what a front block may write,
// but for its sessions and decisions, which stay the Planner's record. Or
// it is an open item the user adds (Item), or strikes (ItemDone).
type frontEdit struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	State    *string   `json:"state"`
	Next     *string   `json:"next"`
	Waiting  *string   `json:"waiting"`
	Close    bool      `json:"close"`
	Item     *itemEdit `json:"item"`
	ItemDone string    `json:"item_done"`
}

// itemEdit is an open item as the user adds it in the left column: under a
// line, or under a workspace's loose ones.
type itemEdit struct {
	Text      string `json:"text"`
	Workspace string `json:"workspace"`
	Front     string `json:"front"`
}

// line reports whether e edits a line of work.
func (e frontEdit) line() bool {
	return e.ID != "" || e.Name != "" || e.State != nil || e.Next != nil || e.Waiting != nil || e.Close
}

// workspaceIDRE is a workspace ID (workspace.Registry's). Whether it is
// registered is not asked: the page offers the ones it lists.
var workspaceIDRE = regexp.MustCompile(`^ws_[a-z0-9]{8}$`)

// HandleFrontEdit applies the user's edit of one line.
func (h *Handler) HandleFrontEdit(w http.ResponseWriter, r *http.Request) {
	if !httputil.DirectLoopback(r) {
		httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "fronts_readonly_remote"})
		return
	}
	if h.fronts == nil {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "no ledger"})
		return
	}
	var e frontEdit
	dec := json.NewDecoder(httputil.WithMaxBytes(w, r, maxFrontBody).Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if e.Item != nil || e.ItemDone != "" {
		if e.line() || e.Item != nil && e.ItemDone != "" {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "一次只做一件事：改工作线、加一条或划掉一条"})
			return
		}
		h.itemEdit(w, e)
		return
	}
	u := front.Update{ID: e.ID, Name: e.Name, State: e.State, Next: e.Next, Waiting: e.Waiting, Close: e.Close}
	if err := u.Validate(); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// The page edits the lines it lists: an unknown ID is a stale page, not
	// a new line (those the Planner makes).
	if !h.fronts.Has(u.ID) {
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "没有这条工作线"})
		return
	}
	f, err := h.fronts.Apply(u)
	switch {
	case errors.Is(err, front.ErrTooMany):
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case err != nil:
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	httputil.WriteJSON(w, f)
}

// itemEdit adds the open item the user wrote, theirs by where it came from
// (the loopback page), or strikes the one they clicked.
func (h *Handler) itemEdit(w http.ResponseWriter, e frontEdit) {
	var (
		it  front.Item
		err error
	)
	if e.Item != nil {
		if e.Item.Workspace != "" && !workspaceIDRE.MatchString(e.Item.Workspace) {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "workspace 要是工作区的 ID"})
			return
		}
		it, _, err = h.fronts.AddItem(front.Item{Text: e.Item.Text, Workspace: e.Item.Workspace, Front: e.Item.Front, From: front.FromUser})
	} else {
		it, err = h.fronts.CloseItem(e.ItemDone, front.ByUser)
	}
	switch {
	case errors.Is(err, front.ErrNoItem), errors.Is(err, front.ErrNoFront):
		httputil.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, front.ErrItemsFull):
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		httputil.WriteJSON(w, it)
	}
}
