package system

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// WorkspaceLister is the consumer-side subset of *workspace.Registry. A nil
// value leaves the health table's workspaces unnamed.
type WorkspaceLister interface {
	List() []workspace.Workspace
}

// healthUnit is one row of GET /api/system/health: the unit plus its
// workspace's name.
type healthUnit struct {
	health.Unit
	WorkspaceName string `json:"workspace_name,omitempty"`
}

type healthResp struct {
	At      time.Time    `json:"at,omitzero"`
	Unowned int          `json:"unowned"`
	Units   []healthUnit `json:"units"`
}

// healthRank orders the table: the states a person may need to act on
// first, then the ones with too little data to tell, then the healthy.
var healthRank = map[health.State]int{
	health.Failed:  0,
	health.Lost:    1,
	health.Stalled: 2,
	health.Waiting: 3,
	health.Unknown: 4,
	health.Active:  5,
	health.Idle:    6,
}

// HandleHealth serves GET /api/system/health: the health state machine's
// table as of its last reconcile (docs/rfc/planner.md §3.8, §10), abnormal
// states first and, within a state, the most recently active first. With no
// state machine wired the table is empty.
func (h *Handlers) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	resp := healthResp{Units: []healthUnit{}}
	if h.health == nil {
		httputil.WriteJSON(w, resp)
		return
	}
	snap := h.health()
	resp.At, resp.Unowned = snap.At, snap.Unowned
	names := map[string]string{}
	if h.workspaces != nil {
		for _, ws := range h.workspaces.List() {
			names[ws.ID] = ws.Name
		}
	}
	for _, u := range snap.Units {
		resp.Units = append(resp.Units, healthUnit{Unit: u, WorkspaceName: names[u.Workspace]})
	}
	slices.SortStableFunc(resp.Units, func(a, b healthUnit) int {
		return cmp.Or(
			cmp.Compare(rankOf(a.State), rankOf(b.State)),
			b.LastActive.Compare(a.LastActive),
		)
	})
	httputil.WriteJSON(w, resp)
}

// rankOf places a state this build does not know with the unknown ones.
func rankOf(s health.State) int {
	if r, ok := healthRank[s]; ok {
		return r
	}
	return healthRank[health.Unknown]
}
