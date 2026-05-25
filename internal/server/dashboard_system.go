// dashboard_system.go — System (sysession) dashboard endpoints.
//
// Two endpoints, both gated by the same auth middleware as the rest of
// /api/*:
//
//	GET  /api/system/daemons             read-only daemon status list
//	POST /api/system/labels/clear-origin reset a session's LabelOrigin
//
// See docs/rfc/system-session.md §9.2 / §9.3.
//
// Phase 1 keeps these handlers thin — no pause/trigger/edit endpoints.
// Operators who need finer-grained control flip cfg.Sysession.* values
// in YAML and restart.  Phase 2 may add controls once we have run-history
// persistence to back them.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

// handleSystemDaemons serves the read-only daemon status list.  Returns
// an empty array (not 404) when sysession is disabled so dashboard JS
// can rely on the response shape.
//
// Encoding goes through a bytes.Buffer first so a marshal error produces
// a clean 500 rather than the ResponseWriter footgun where Encode has
// already streamed bytes (header sent, status frozen at 200) before the
// error path tries to upgrade the response.
//
// R246-SEC-3 [BREAKING-LOCAL]: route success bodies through
// writeJSONBytes so the X-Content-Type-Options / Cache-Control headers
// match the rest of /api/*.  Without nosniff a legacy browser MIME-
// sniffing path could re-interpret the JSON as HTML; without
// no-store a shared proxy would cache another operator's daemon
// snapshot. The breaking surface is purely additive (extra response
// headers); same wire body.
func (s *Server) handleSystemDaemons(w http.ResponseWriter, _ *http.Request) {
	if s.sysessionMgr == nil {
		// Empty array preserves the "GET always returns JSON array"
		// contract for the dashboard polling loop.
		writeJSONBytes(w, []byte("[]"))
		return
	}
	statuses := s.sysessionMgr.Inspector()
	if statuses == nil {
		statuses = []sysession.DaemonStatus{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(statuses); err != nil {
		http.Error(w, "encode daemon list", http.StatusInternalServerError)
		return
	}
	writeJSONBytes(w, buf.Bytes())
}

// clearLabelOriginRequest is the POST body for /api/system/labels/clear-origin.
type clearLabelOriginRequest struct {
	Key string `json:"key"`
}

// handleClearLabelOrigin clears the LabelOrigin (and the UserLabel)
// for a single session so the AutoTitler daemon can rename it again.
//
// Body: {"key": "<session-key>"}.
//
// Returns 200 with {"ok": true} on success, 400 for missing/invalid
// keys, 404 when the key is unknown.
func (s *Server) handleClearLabelOrigin(w http.ResponseWriter, r *http.Request) {
	// Cap the body so a multi-MB hostile payload cannot be buffered
	// before json.Decoder surfaces an error; mirrors every other
	// dashboard mutation endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req clearLabelOriginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	// Session-level rule: this endpoint is for IM sessions, not the
	// reserved namespaces (cron / project / scratch / sys).  Those are
	// managed via their own UIs (or have no operator-facing labels at
	// all).  Reject early to give a clear error rather than letting the
	// router silently no-op on a stub that can't carry a user label.
	if session.IsReservedNamespace(req.Key) {
		http.Error(w, "label-origin only applies to user sessions", http.StatusBadRequest)
		return
	}
	if !s.router.ClearUserLabelOrigin(req.Key) {
		http.NotFound(w, r)
		return
	}
	// R246-SEC-3 [BREAKING-LOCAL]: writeJSONBytes routes through the same
	// header set (nosniff + no-store) used by every other /api/* mutation
	// success path. The body shape `{"ok":true}` differs from writeOK's
	// `{"status":"ok"}` so we keep the bytes literal here rather than
	// switching to writeOK, which would change the wire contract.
	writeJSONBytes(w, []byte(`{"ok":true}`))
}
