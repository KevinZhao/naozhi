// Package planner hosts the dashboard endpoints of the global Planner
// (internal/planner/agent, docs/rfc/planner.md §15.1):
//
//	GET  /api/planner        status: planner.enabled, the stop switch, the session
//	POST /api/planner/state  {"stopped": bool} — the runtime "停用 Planner" switch
//	POST /api/planner/send   {"text": "...", "ref": "<session key>"} — a message
//	                         to the Planner (ref: a session handed over with
//	                         it), the gateway's snapshot before it and a receipt
//	GET  /api/planner/briefs the latest morning briefs, newest first (?limit=)
//	GET  /api/planner/notes  the gateway's journaled notes in the conversation
//	                         (due reminders, dispatch outcomes), oldest first
//
// All sit behind the /api/* auth middleware.
package planner

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Handler serves the endpoints over an *agent.Agent; a nil agent answers
// every request as planner.enabled off. The briefs do not depend on the
// Planner: they are listed whether it is on or not.
type Handler struct {
	agent  *agent.Agent
	briefs *brief.Briefer
}

// New returns a Handler for a and b (either nil when not built).
func New(a *agent.Agent, b *brief.Briefer) *Handler { return &Handler{agent: a, briefs: b} }

// Brief list sizes: the default and the most one request returns.
const (
	briefsDefault = 7
	briefsMax     = 14
)

// HandleBriefs lists the latest morning briefs, newest first.
func (h *Handler) HandleBriefs(w http.ResponseWriter, r *http.Request) {
	limit := briefsDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = min(n, briefsMax)
	}
	out := []brief.Brief{}
	if h.briefs != nil {
		out = h.briefs.Recent(limit)
	}
	httputil.WriteJSON(w, map[string]any{"briefs": out})
}

// HandleNotes lists the gateway's notes in the Planner conversation; the
// page merges them into the event stream, whose history a restart drops.
func (h *Handler) HandleNotes(w http.ResponseWriter, _ *http.Request) {
	out := []agent.Note{}
	if h.agent != nil {
		var err error
		if out, err = h.agent.Notes(); err != nil {
			slog.Warn("planner notes: journal read incomplete", "err", err)
		}
	}
	httputil.WriteJSON(w, map[string]any{"notes": out})
}

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
		// Ref is a local session key handed over with the text (交给 Planner).
		Ref string `json:"ref"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Ref != "" && sessionkey.ValidateSessionKey(req.Ref) != nil {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid ref"})
		return
	}
	if h.agent == nil {
		writeErr(w, agent.ErrOff)
		return
	}
	if err := h.agent.Send(req.Text, req.Ref); err != nil {
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
	case errors.Is(err, agent.ErrNoRef):
		status = http.StatusNotFound
	case errors.Is(err, agent.ErrNotReady):
	default:
		slog.Warn("planner send failed", "err", err)
	}
	httputil.WriteJSONStatus(w, status, map[string]string{"error": err.Error()})
}
