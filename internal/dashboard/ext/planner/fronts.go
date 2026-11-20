package planner

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/front"
)

// The ledger of the user's lines of work on the Planner page (planner RFC
// §15.19, D134):
//
//	GET  /api/planner/fronts  the lines in progress, the latest updated
//	                          first, and whether this request may edit them
//	POST /api/planner/fronts  {"id", "name", "state", "next", "waiting",
//	                          "close"} — the user's own edit of one line
//	                          (a field left out stays, "" clears it); only
//	                          straight from loopback, 403 otherwise
//
// The read sits behind the /api/* auth like the rest of the page. Neither
// route is among a reverse node's `api` routes (node.APIRoutes): the ledger
// is shown and edited on the machine that keeps it.

// maxFrontBody caps an edit's body: a few short fields.
const maxFrontBody = 16 << 10

// SetFronts wires the ledger; nil (no Planner state root) lists none.
func (h *Handler) SetFronts(s *front.Store) { h.fronts = s }

type frontsResponse struct {
	Fronts []front.Front `json:"fronts"`
	// Writable is whether an edit from this request would be taken
	// (httputil.DirectLoopback); the page offers none otherwise.
	Writable bool `json:"writable"`
}

// HandleFronts lists the lines of work in progress.
func (h *Handler) HandleFronts(w http.ResponseWriter, r *http.Request) {
	resp := frontsResponse{Fronts: []front.Front{}, Writable: h.fronts != nil && httputil.DirectLoopback(r)}
	if h.fronts != nil {
		if open := h.fronts.Open(); open != nil {
			resp.Fronts = open
		}
	}
	httputil.WriteJSON(w, resp)
}

// frontEdit is the user's edit of one line: what a front block may write,
// but for its sessions and decisions, which stay the Planner's record.
type frontEdit struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	State   *string `json:"state"`
	Next    *string `json:"next"`
	Waiting *string `json:"waiting"`
	Close   bool    `json:"close"`
}

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
