// Package custody hosts the custody endpoints over internal/planner/custody
// (docs/rfc/planner.md §5.4, D93):
//
//	GET    /api/custody         the pause and every session in custody; ?key= one session's custody
//	PUT    /api/custody         turn a session's custody on or off as the user ({"key","on"})
//	PUT    /api/custody/pause   pause custody for every session
//	DELETE /api/custody/pause   resume it
//
// All sit behind the /api/* auth middleware: a request here is the user.
// Only this node's sessions are in custody; a remote node's ("node" set to
// another node) is refused.
package custody

import (
	"errors"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/custody"
)

// Source is custody (custody.Engine).
type Source interface {
	Status(key string) custody.Status
	List() []custody.Status
	Set(key string, on bool, by string) error
	Paused() (bool, time.Time)
	SetPaused(paused bool) error
}

// Handler serves the endpoints. A nil source lists nothing and refuses
// changes with 503.
type Handler struct {
	src Source
}

// New returns a Handler over src.
func New(src Source) *Handler { return &Handler{src: src} }

type pauseResp struct {
	Paused   bool      `json:"paused"`
	PausedAt time.Time `json:"paused_at,omitzero"`
}

type listResp struct {
	pauseResp
	Sessions []custody.Status `json:"sessions"`
}

type oneResp struct {
	pauseResp
	Session custody.Status `json:"session"`
}

// HandleList serves GET /api/custody.
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	var p pauseResp
	if h.src != nil {
		p.Paused, p.PausedAt = h.src.Paused()
	}
	key := r.URL.Query().Get("key")
	switch {
	case key != "" && h.src != nil:
		httputil.WriteJSON(w, oneResp{pauseResp: p, Session: h.src.Status(key)})
	case key != "":
		httputil.WriteJSON(w, oneResp{pauseResp: p, Session: custody.Status{Key: key, State: custody.StateOff, Max: custody.MaxSends}})
	default:
		resp := listResp{pauseResp: p, Sessions: []custody.Status{}}
		if h.src != nil {
			if l := h.src.List(); l != nil {
				resp.Sessions = l
			}
		}
		httputil.WriteJSON(w, resp)
	}
}

// HandleSet serves PUT /api/custody: the header's one-click switch.
func (h *Handler) HandleSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
		On   *bool  `json:"on"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" || req.On == nil {
		http.Error(w, `body must be {"key","on"}`, http.StatusBadRequest)
		return
	}
	if req.Node != "" && req.Node != "local" {
		http.Error(w, "远程节点的会话不能托管", http.StatusBadRequest)
		return
	}
	if h.src == nil {
		http.Error(w, "custody not available", http.StatusServiceUnavailable)
		return
	}
	switch err := h.src.Set(req.Key, *req.On, custody.ByUser); {
	case errors.Is(err, custody.ErrNoSession):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, custody.ErrRefused):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, custody.ErrDisabled):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		httputil.WriteJSON(w, h.src.Status(req.Key))
	}
}

// HandlePause serves PUT /api/custody/pause.
func (h *Handler) HandlePause(w http.ResponseWriter, _ *http.Request) { h.pause(w, true) }

// HandleResume serves DELETE /api/custody/pause.
func (h *Handler) HandleResume(w http.ResponseWriter, _ *http.Request) { h.pause(w, false) }

func (h *Handler) pause(w http.ResponseWriter, paused bool) {
	if h.src == nil {
		http.Error(w, "custody not available", http.StatusServiceUnavailable)
		return
	}
	if err := h.src.SetPaused(paused); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p, at := h.src.Paused()
	httputil.WriteJSON(w, pauseResp{Paused: p, PausedAt: at})
}
