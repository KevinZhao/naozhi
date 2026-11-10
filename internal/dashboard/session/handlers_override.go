package session

// handlers_override.go — POST /api/sessions/override: per-session model /
// effort tuning from the dashboard header chips
// (docs/rfc/dashboard-model-effort-control.md §4), and the switch of a
// session's access profile (docs/rfc/project-access-profile.md §7.1).

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// overrideRequest is the wire shape. Model/Effort use pointers so the three
// states are distinguishable: absent (leave unchanged), "" (clear the
// override, config chain reapplies), value (set).
//
// AccessProfile, when present, switches the session's access profile ("" =
// the global credentials), with Model riding along; Effort cannot.
type overrideRequest struct {
	Key           string  `json:"key"`
	Node          string  `json:"node"`
	Model         *string `json:"model"`
	Effort        *string `json:"effort"`
	AccessProfile *string `json:"access_profile"`
}

// HandleOverride applies a per-session tuning override and responds
// {"applied_via":"rpc"|"respawn"|"deferred"}; the dashboard renders its
// pending-state hint from applied_via. Errors: 400 validation (bad key /
// flag-shaped model / out-of-set or unsupported effort / no fields); 404
// unknown key; 409 CLI rejected the switch (body carries the CLI's own,
// already-sanitized text; nothing was recorded) or an access-profile switch
// met a running turn; 501 remote-node session
// (override is local-only; the popover greys these out).
func (h *Handlers) HandleOverride(w http.ResponseWriter, r *http.Request) {
	var req overrideRequest
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// Gate req.Key before it reaches slog attrs or router lookups.
	if err := sessionkey.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	if req.Node != "" && req.Node != "local" {
		// 501 (not 400) so a future remote-capable dashboard can distinguish
		// "unsupported" from a malformed request.
		http.Error(w, "session tuning is not supported for remote-node sessions yet", http.StatusNotImplemented)
		return
	}

	if req.AccessProfile != nil {
		h.switchAccessProfile(w, r, req)
		return
	}

	appliedVia, err := h.deps.Router.SetSessionTuning(r.Context(), req.Key, req.Model, req.Effort)
	if err != nil {
		switch {
		case errors.Is(err, clierr.ErrSetModelRejected):
			// CLI text is safe: sanitized at the protocol layer
			// (parseControlAck / ACP interception) before it entered the
			// error chain.
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			// Validation family: the message may echo arbitrary operator input,
			// so sanitize it before returning.
			http.Error(w, sessionkey.SanitizeLogAttr(err.Error()), http.StatusBadRequest)
		}
		return
	}

	slog.Info("session tuning override",
		"key", sessionkey.SanitizeLogAttr(req.Key),
		"applied_via", appliedVia,
		"model_set", req.Model != nil,
		"effort_set", req.Effort != nil)
	httputil.WriteJSON(w, map[string]string{"applied_via": appliedVia})
}

// switchAccessProfile is HandleOverride for a request naming access_profile.
func (h *Handlers) switchAccessProfile(w http.ResponseWriter, r *http.Request, req overrideRequest) {
	if req.Effort != nil {
		http.Error(w, "effort cannot change together with the access profile", http.StatusBadRequest)
		return
	}
	appliedVia, err := h.deps.Router.SwitchAccessProfile(r.Context(), req.Key, *req.AccessProfile, req.Model)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, sessionpkg.ErrAccessProfileSwitchBusy) {
			code = http.StatusConflict
		}
		http.Error(w, sessionkey.SanitizeLogAttr(err.Error()), code)
		return
	}
	slog.Info("session access profile override",
		"key", sessionkey.SanitizeLogAttr(req.Key),
		"access_profile", sessionkey.SanitizeLogAttr(*req.AccessProfile),
		"applied_via", appliedVia,
		"model_set", req.Model != nil)
	httputil.WriteJSON(w, map[string]string{"applied_via": appliedVia})
}
