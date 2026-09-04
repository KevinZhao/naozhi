// Package uisettings hosts the dashboard /api/settings endpoints —
// instance-wide UI preferences (theme), backed by internal/uiprefs.
//
//	GET /api/settings   read the instance-wide UI preferences (theme)
//	PUT /api/settings   replace them
//
// Both sit behind the /api/* auth middleware. naozhi is single-user, so these
// read/write one instance-wide document; the browser's localStorage copy is
// only a first-paint cache.
package uisettings

import (
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/uiprefs"
)

// Handler serves the /api/settings endpoint pair backed by a *uiprefs.Store.
// A nil store degrades gracefully (defaults on GET, 503 on PUT).
type Handler struct {
	store *uiprefs.Store
}

// New returns a Handler backed by store (uiprefs.New("") gives in-memory).
func New(store *uiprefs.Store) *Handler {
	return &Handler{store: store}
}

// HandleGet serves the current UI preferences (defaults when never saved).
func (h *Handler) HandleGet(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		// Defensive: a hand-built test Server may have no store; emit the default shape.
		httputil.WriteJSON(w, uiprefs.Settings{Theme: "auto"})
		return
	}
	httputil.WriteJSON(w, h.store.Get())
}

// HandlePut replaces the UI preferences ({"theme":"dark"}). Store.Set
// normalises unknown themes, so validation here is body size + JSON shape.
func (h *Handler) HandlePut(w http.ResponseWriter, r *http.Request) {
	// Cap the body before decoding; DecodeJSONBody relies on the caller wrapping
	// r.Body and adds the DisallowUnknownFields mass-assignment guard.
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req uiprefs.Settings
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if h.store == nil {
		// No store wired (test Server): report persistence unavailable rather than drop.
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
