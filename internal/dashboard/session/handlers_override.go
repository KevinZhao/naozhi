package session

// handlers_override.go — POST /api/sessions/override: per-session model /
// effort tuning from the dashboard header chips.
// docs/rfc/dashboard-model-effort-control.md §4.3 (API shape) / §4.1
// (applied_via drives the popover hint).

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/cli"
	sessionpkg "github.com/naozhi/naozhi/internal/session"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
)

// overrideRequest is the wire shape. Model/Effort use pointers so the three
// states are distinguishable: absent (leave unchanged), "" (clear the
// override, config chain reapplies), value (set).
type overrideRequest struct {
	Key    string  `json:"key"`
	Node   string  `json:"node"`
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
}

// HandleOverride applies a per-session tuning override. Responds
// {"applied_via":"rpc"|"respawn"|"deferred"} on success; the dashboard
// renders its pending-state hint from applied_via rather than re-deriving
// the F9 path split client-side.
//
// Error mapping:
//   - 400: validation (bad key / flag-shaped model / out-of-set effort /
//     effort on a non-EffortTier backend / no fields).
//   - 404: unknown session key.
//   - 409: CLI rejected the switch (claude org policy F7 / unknown model
//     F15). Body carries the CLI's own text — already sanitized at the
//     protocol layer — for the dashboard toast. Nothing was recorded
//     (§6 R8).
//   - 501: remote-node session (NG4: override is local-only in this slice;
//     the popover greys these out, so this is a defence for direct callers).
func (h *Handlers) HandleOverride(w http.ResponseWriter, r *http.Request) {
	var req overrideRequest
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// R175-SEC-M: gate req.Key before it reaches slog attrs or router
	// lookups. Same policy as HandleEvents / HandleDelete / HandleSetLabel.
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	if req.Node != "" && req.Node != "local" {
		// NG4: no node-protocol forwarding in this slice. 501 (not 400) so a
		// future remote-capable dashboard can distinguish "server too old /
		// unsupported" from a malformed request.
		http.Error(w, "session tuning is not supported for remote-node sessions yet", http.StatusNotImplemented)
		return
	}

	appliedVia, err := h.router.SetSessionTuning(r.Context(), req.Key, req.Model, req.Effort)
	if err != nil {
		switch {
		case errors.Is(err, sessionpkg.ErrTuningUnknownSession):
			http.Error(w, "session not found", http.StatusNotFound)
		case errors.Is(err, cli.ErrSetModelRejected):
			// CLI text is safe: sanitized at the protocol layer
			// (parseControlAck / ACP interception) before it entered the
			// error chain.
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			// Validation family (tuningspec / ErrTuningEffortUnsupported /
			// no fields). tuningspec messages name the offending value but
			// were validated to be printable by construction? No — the
			// offending value is operator input and may be arbitrary bytes,
			// so sanitize the whole message before echoing.
			http.Error(w, sessionpkg.SanitizeLogAttr(err.Error()), http.StatusBadRequest)
		}
		return
	}

	slog.Info("session tuning override",
		"key", sessionpkg.SanitizeLogAttr(req.Key),
		"applied_via", appliedVia,
		"model_set", req.Model != nil,
		"effort_set", req.Effort != nil)
	httputil.WriteJSON(w, map[string]string{"applied_via": appliedVia})
}
