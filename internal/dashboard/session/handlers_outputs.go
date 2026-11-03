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
// library). A file's root + path go to /api/projects/file as they are.
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
	httputil.WriteJSON(w, map[string]any{"outputs": outs})
}
