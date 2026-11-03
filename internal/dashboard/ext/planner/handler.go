// Package planner hosts the dashboard endpoints of the global Planner
// (internal/planner/agent, docs/rfc/planner.md §15.1):
//
//	GET  /api/planner        status: planner.enabled, the stop switch, the session
//	POST /api/planner/state  {"stopped": bool} — the runtime "停用 Planner" switch
//	POST /api/planner/send   {"text": "..."} — a message to the Planner, with
//	                         the gateway's snapshot before it and a receipt
//
// All sit behind the /api/* auth middleware.
package planner

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/agent"
)

// Handler serves the endpoints over an *agent.Agent; a nil agent answers
// every request as planner.enabled off.
type Handler struct {
	agent *agent.Agent
}

// New returns a Handler for a (nil when not built).
func New(a *agent.Agent) *Handler { return &Handler{agent: a} }

// HandleStatus reports the Planner's switch and session.
func (h *Handler) HandleStatus(w http.ResponseWriter, _ *http.Request) {
	if h.agent == nil {
		httputil.WriteJSON(w, agent.Status{Key: agent.Key})
		return
	}
	httputil.WriteJSON(w, h.agent.Status())
}

// HandleState flips the runtime stop switch.
func (h *Handler) HandleState(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		Stopped *bool `json:"stopped"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Stopped == nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "stopped is required"})
		return
	}
	if h.agent == nil {
		writeErr(w, agent.ErrOff)
		return
	}
	if err := h.agent.SetStopped(*req.Stopped); err != nil {
		writeErr(w, err)
		return
	}
	httputil.WriteJSON(w, h.agent.Status())
}

// HandleSend takes a message to the Planner. 202 once the turn is accepted;
// the receipt and the answer arrive on the planner:main event stream.
func (h *Handler) HandleSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var req struct {
		Text string `json:"text"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if h.agent == nil {
		writeErr(w, agent.ErrOff)
		return
	}
	if err := h.agent.Send(req.Text); err != nil {
		writeErr(w, err)
		return
	}
	httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": agent.Key})
}

// writeErr maps the agent's errors to statuses; their text is user-facing.
func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, agent.ErrOff), errors.Is(err, agent.ErrStopped):
		status = http.StatusConflict
	case errors.Is(err, agent.ErrEmpty), errors.Is(err, agent.ErrTooLong):
		status = http.StatusBadRequest
	case errors.Is(err, agent.ErrNotReady):
	default:
		slog.Warn("planner send failed", "err", err)
	}
	httputil.WriteJSONStatus(w, status, map[string]string{"error": err.Error()})
}
