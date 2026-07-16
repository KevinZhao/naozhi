// Package uisettings hosts the dashboard /api/settings endpoints —
// instance-wide UI preferences (theme), backed by internal/uiprefs.
//
// Moved from internal/server (dashboard_uiprefs.go) per lint rule 1
// (server-split-phase4-design.md §9.2: no new *Server handle* methods
// after Phase 0; unplanned violations move to a dashboard sub-package).
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
package uisettings

import (
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/uiprefs"
)

// Handler serves the /api/settings endpoint pair backed by a
// *uiprefs.Store. A nil store degrades gracefully (defaults on GET,
// 503 on PUT) so a hand-built test Server without wiring keeps the
// pre-move defensive semantics.
type Handler struct {
	store *uiprefs.Store
}

// New returns a Handler backed by store. Callers normally pass
// uiprefs.New(stateDir); an empty stateDir yields an in-memory store,
// so persistence-less test harnesses need no special casing.
func New(store *uiprefs.Store) *Handler {
	return &Handler{store: store}
}

// HandleGet serves the current UI preferences. Always returns the
// document (defaults when nothing was ever saved), so the dashboard can
// rely on a stable response shape.
func (h *Handler) HandleGet(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		// Defensive: buildServer always wires a (possibly in-memory) store,
		// but a hand-built test Server might not. Emit the same default shape.
		httputil.WriteJSON(w, uiprefs.Settings{Theme: "auto"})
		return
	}
	httputil.WriteJSON(w, h.store.Get())
}

// HandlePut replaces the UI preferences. Body: a uiprefs.Settings
// JSON object, e.g. {"theme":"dark"}. uiprefs.Store.Set normalises an unknown
// theme to the default, so validation here is limited to body-size + JSON
// shape; an invalid theme is accepted-and-normalised rather than rejected,
// matching the lenient localStorage behaviour the dashboard had before.
func (h *Handler) HandlePut(w http.ResponseWriter, r *http.Request) {
	// Cap the body before decoding; DecodeJSONBody relies on the caller
	// wrapping r.Body (and adds the package-wide DisallowUnknownFields
	// mass-assignment guard).
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req uiprefs.Settings
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if h.store == nil {
		// No store wired (test Server): accept the request shape but report
		// that persistence isn't available rather than silently dropping it.
		http.Error(w, "ui settings store not configured", http.StatusServiceUnavailable)
		return
	}
	if err := h.store.Set(req); err != nil {
		slog.Error("ui settings save failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	httputil.WriteOK(w)
}
