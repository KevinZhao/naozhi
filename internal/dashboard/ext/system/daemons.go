package system

import (
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/session"
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

// clearLabelOriginRequest is the POST body for /api/system/labels/clear-origin.
type clearLabelOriginRequest struct {
	Key string `json:"key"`
}

// HandleClearLabelOrigin clears the LabelOrigin (and the UserLabel) for a
// single session so the AutoTitler daemon can rename it again.
//
// Body: {"key": "<session-key>"}. Returns 200 with {"status":"ok"} on
// success, 400 for missing/invalid keys, 404 when the key is unknown.
func (h *Handlers) HandleClearLabelOrigin(w http.ResponseWriter, r *http.Request) {
	// DecodeJSONBody does NOT cap the body itself; callers must wrap r.Body.
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req clearLabelOriginRequest
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if req.Key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if err := session.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	// Reserved namespaces (cron / project / scratch / sys) cannot carry a
	// user label; reject early with a clear error.
	if session.IsReservedNamespace(req.Key) {
		http.Error(w, "label-origin only applies to user sessions", http.StatusBadRequest)
		return
	}
	if !h.router.ClearUserLabelOrigin(req.Key) {
		http.NotFound(w, r)
		return
	}
	httputil.WriteOK(w)
}
