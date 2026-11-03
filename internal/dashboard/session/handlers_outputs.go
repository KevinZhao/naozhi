package session

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
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
// would resolve to the wrong tree), only its dir + path.
func (h *Handlers) HandleOutputs(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key != "" {
		if err := sessionkey.ValidateSessionKey(key); err != nil {
			http.Error(w, "invalid key parameter", http.StatusBadRequest)
			return
		}
	}
	outs := []activity.Output{}
	if h.deps.Outputs != nil {
		outs = h.deps.Outputs.Outputs(key, maxOutputsListed)
	}
	for i := range outs {
		if outs[i].Root != "" && !h.sessionWorksIn(outs[i].Key, outs[i].Dir) {
			outs[i].Root = ""
		}
	}
	httputil.WriteJSON(w, map[string]any{"outputs": outs})
}

// sessionWorksIn reports whether the local session key's workspace is dir.
func (h *Handlers) sessionWorksIn(key, dir string) bool {
	if h.deps.Router == nil || dir == "" {
		return false
	}
	sess := h.deps.Router.SessionFor(key)
	return sess != nil && sess.Workspace() == dir
}
