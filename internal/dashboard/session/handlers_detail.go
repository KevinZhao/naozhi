package session

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// WorkspaceLookup is the workspace registry (planner/workspace.Registry).
type WorkspaceLookup interface {
	Lookup(dir string) (workspace.Workspace, bool)
}

// workspaceRef names the registered workspace a session works in.
type workspaceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// sessionDetail is what the workbench 详情 tab shows beyond the
// /api/sessions snapshot: the cross-session name, the registered workspace
// and the health unit, each omitted when unknown.
type sessionDetail struct {
	Key       string        `json:"key"`
	Name      string        `json:"name,omitempty"`
	Workspace *workspaceRef `json:"workspace,omitempty"`
	Health    *health.Unit  `json:"health,omitempty"`
}

// HandleDetail serves GET /api/sessions/detail?key= for a local session.
func (h *Handlers) HandleDetail(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := sessionkey.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	d := sessionDetail{Key: key}
	if h.deps.Router != nil {
		if sess := h.deps.Router.SessionFor(key); sess != nil {
			d.Name = sess.CLISessionName()
			if h.deps.Workspaces != nil {
				if ws, ok := h.deps.Workspaces.Lookup(sess.Workspace()); ok {
					d.Workspace = &workspaceRef{ID: ws.ID, Name: ws.Name}
				}
			}
		}
	}
	if h.deps.Health != nil {
		for _, u := range h.deps.Health().Units {
			if u.ID == key {
				d.Health = &u
				break
			}
		}
	}
	httputil.WriteJSON(w, d)
}
