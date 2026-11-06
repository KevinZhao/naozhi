// Package custody hosts the custody endpoints over internal/planner/custody
// (docs/rfc/planner.md §5.4, D93):
//
//	GET    /api/custody         the pause and every session in custody; ?key= one session's custody
//	PUT    /api/custody         turn a session's custody on or off as the user ({"key","on"})
//	PUT    /api/custody/pause   the inbox pause's alias: pause custody and pushes ({"for"}, default "today")
//	DELETE /api/custody/pause   resume both
//	POST   /api/custody/undo    take back the answer custody gave on the user's behalf ({"key"})
//
// All sit behind the /api/* auth middleware: a request here is the user.
// Only this node's sessions are in custody; a remote node's ("node" set to
// another node) is refused.
package custody

import (
	"cmp"
	"errors"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/custody"
)

// Source is custody (custody.Engine).
type Source interface {
	Status(key string) custody.Status
	List() []custody.Status
	Set(key string, on bool, by string) error
	// Paused is the one pause (暂停推送与托管) and when it ends.
	Paused() (bool, time.Time)
	Pause(until time.Time) error
	Undo(key string) (string, error)
}

// Handler serves the endpoints. A nil source lists nothing and refuses
// changes with 503.
type Handler struct {
	src Source
}

// New returns a Handler over src.
func New(src Source) *Handler { return &Handler{src: src} }

type pauseResp struct {
	Paused      bool      `json:"paused"`
	PausedUntil time.Time `json:"paused_until,omitzero"`
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
		p.Paused, p.PausedUntil = h.src.Paused()
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

// HandlePause serves PUT /api/custody/pause: the same pause as PUT
// /api/inbox/pause, for as long as its body's "for" says, today without one.
func (h *Handler) HandlePause(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		For string `json:"for"`
	}
	if r.ContentLength != 0 {
		if err := httputil.DecodeJSONBody(r, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	until, err := notify.ParsePause(cmp.Or(req.For, "today"), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.pause(w, until)
}

// HandleResume serves DELETE /api/custody/pause.
func (h *Handler) HandleResume(w http.ResponseWriter, _ *http.Request) { h.pause(w, time.Time{}) }

func (h *Handler) pause(w http.ResponseWriter, until time.Time) {
	if h.src == nil {
		http.Error(w, "custody not available", http.StatusServiceUnavailable)
		return
	}
	if err := h.src.Pause(until); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p, end := h.src.Paused()
	httputil.WriteJSON(w, pauseResp{Paused: p, PausedUntil: end})
}

// HandleUndo serves POST /api/custody/undo: the 撤回 of a session's latest
// answer custody gave for a recommended option. It answers the answer taken
// back.
func (h *Handler) HandleUndo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, `body must be {"key"}`, http.StatusBadRequest)
		return
	}
	if req.Node != "" && req.Node != "local" {
		http.Error(w, "远程节点的会话不在托管中", http.StatusBadRequest)
		return
	}
	if h.src == nil {
		http.Error(w, "custody not available", http.StatusServiceUnavailable)
		return
	}
	answer, err := h.src.Undo(req.Key)
	switch {
	case errors.Is(err, custody.ErrNoSession), errors.Is(err, custody.ErrNoSuggested):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, custody.ErrRefused):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, custody.ErrDisabled):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		httputil.WriteJSON(w, struct {
			Answer string `json:"answer"`
		}{answer})
	}
}
