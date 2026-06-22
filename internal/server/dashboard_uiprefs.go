// dashboard_uiprefs.go — UI-preferences dashboard endpoints.
//
// Two endpoints, gated by the same auth middleware as the rest of /api/*:
//
//	GET /api/settings   read the instance-wide UI preferences (theme)
//	PUT /api/settings   replace them
//
// naozhi is single-user (the auth cookie carries no per-session identity —
// internal/dashboard/auth/handlers.go), so these read/write one instance-wide
// document held by internal/uiprefs. The dashboard previously kept theme only
// in browser localStorage; persisting it server-side lets the choice survive
// a browser switch, a new device, or a cache clear. The browser keeps a
// localStorage copy purely as a first-paint cache to avoid a theme flash
// before this GET resolves.
package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/uiprefs"
)

// handleUISettingsGet serves the current UI preferences. Always returns the
// document (defaults when nothing was ever saved), so the dashboard can rely
// on a stable response shape. A nil store (StateDir unset) still returns
// defaults via the in-memory store wired in buildServer.
func (s *Server) handleUISettingsGet(w http.ResponseWriter, _ *http.Request) {
	if s.uiPrefs == nil {
		// Defensive: buildServer always wires a (possibly in-memory) store,
		// but a hand-built test Server might not. Emit the same default shape.
		writeJSON(w, uiprefs.Settings{Theme: "auto"})
		return
	}
	writeJSON(w, s.uiPrefs.Get())
}

// handleUISettingsPut replaces the UI preferences. Body: a uiprefs.Settings
// JSON object, e.g. {"theme":"dark"}. uiprefs.Store.Set normalises an unknown
// theme to the default, so validation here is limited to body-size + JSON
// shape; an invalid theme is accepted-and-normalised rather than rejected,
// matching the lenient localStorage behaviour the dashboard had before.
func (s *Server) handleUISettingsPut(w http.ResponseWriter, r *http.Request) {
	// Cap the body before decoding; decodeJSONBody relies on the caller
	// wrapping r.Body (and adds the package-wide DisallowUnknownFields
	// mass-assignment guard), mirroring handleClearLabelOrigin.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req uiprefs.Settings
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if s.uiPrefs == nil {
		// No store wired (test Server): accept the request shape but report
		// that persistence isn't available rather than silently dropping it.
		http.Error(w, "ui settings store not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.uiPrefs.Set(req); err != nil {
		s.log().Error("ui settings save failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeOK(w)
}
