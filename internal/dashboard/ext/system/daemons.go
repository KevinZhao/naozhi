package system

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/sysession"
)

// HandleDaemons serves the read-only daemon status list. Returns an empty
// array (not 404) when sysession is disabled so dashboard JS can rely on the
// response shape. Always via WriteJSON so every reply carries the nosniff +
// no-store headers.
func (h *Handlers) HandleDaemons(w http.ResponseWriter, _ *http.Request) {
	if h.daemons == nil {
		httputil.WriteJSON(w, []sysession.DaemonStatus{})
		return
	}
	statuses := h.daemons.Inspector()
	if statuses == nil {
		statuses = []sysession.DaemonStatus{}
	}
	httputil.WriteJSON(w, statuses)
}
