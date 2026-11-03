// Package inbox hosts the "等你" endpoints over internal/notify
// (docs/rfc/planner.md §10.1, §15.1).
//
//	GET    /api/inbox         open items and the recently closed, with the pause
//	POST   /api/inbox/close   close an open item ({"id"})
//	POST   /api/inbox/decide  answer a decision item ({"id","action"})
//	PUT    /api/inbox/pause   pause pushes ({"for":"1h"|"today"|<duration>})
//	DELETE /api/inbox/pause   resume pushes
//
// All sit behind the /api/* auth middleware.
package inbox

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/notify"
)

// recentClosed is how many closed items GET /api/inbox lists.
const recentClosed = 50

// maxPause bounds a custom pause.
const maxPause = 7 * 24 * time.Hour

// Source is the inbox (notify.Notifier).
type Source interface {
	List(limit int) (open, closed []notify.Item)
	Counts() notify.Counts
	Dismiss(id string) error
	Decide(id string, a notify.Action) error
	PausedUntil() time.Time
	Pause(until time.Time)
}

// Handler serves the inbox endpoints. A nil source lists nothing and
// refuses changes with 503.
type Handler struct {
	src Source
	now func() time.Time
}

// New returns a Handler over src.
func New(src Source) *Handler { return &Handler{src: src, now: time.Now} }

// listResp's Counts are the first value of the inbox_count frames.
type listResp struct {
	Open        []notify.Item `json:"open"`
	Closed      []notify.Item `json:"closed"`
	Counts      notify.Counts `json:"counts"`
	PausedUntil time.Time     `json:"paused_until,omitzero"`
}

// HandleList serves GET /api/inbox.
func (h *Handler) HandleList(w http.ResponseWriter, _ *http.Request) {
	resp := listResp{Open: []notify.Item{}, Closed: []notify.Item{}}
	if h.src != nil {
		open, closed := h.src.List(recentClosed)
		if open != nil {
			resp.Open = open
		}
		if closed != nil {
			resp.Closed = closed
		}
		resp.Counts = h.src.Counts()
		resp.PausedUntil = h.src.PausedUntil()
	}
	httputil.WriteJSON(w, resp)
}

// HandleClose serves POST /api/inbox/close.
func (h *Handler) HandleClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	h.reply(w, h.src.Dismiss(req.ID))
}

// HandleDecide serves POST /api/inbox/decide.
func (h *Handler) HandleDecide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string        `json:"id"`
		Action notify.Action `json:"action"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	h.reply(w, h.src.Decide(req.ID, req.Action))
}

// HandlePause serves PUT /api/inbox/pause: "1h", "today" (until local
// midnight) or a Go duration up to a week.
func (h *Handler) HandlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		For string `json:"for"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	now := h.now()
	var until time.Time
	if req.For == "today" {
		until = notify.EndOfToday(now)
	} else {
		d, err := time.ParseDuration(req.For)
		if err != nil || d <= 0 || d > maxPause {
			http.Error(w, `"for" must be "today" or a duration up to 168h`, http.StatusBadRequest)
			return
		}
		until = now.Add(d)
	}
	h.src.Pause(until)
	httputil.WriteJSON(w, map[string]any{"paused_until": until})
}

// HandleResume serves DELETE /api/inbox/pause.
func (h *Handler) HandleResume(w http.ResponseWriter, _ *http.Request) {
	if h.src == nil {
		http.Error(w, "inbox not available", http.StatusServiceUnavailable)
		return
	}
	h.src.Pause(time.Time{})
	httputil.WriteOK(w)
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, dst); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	if h.src == nil {
		http.Error(w, "inbox not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *Handler) reply(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		httputil.WriteOK(w)
	case errors.Is(err, notify.ErrNotFound):
		http.Error(w, "no such item", http.StatusNotFound)
	case errors.Is(err, notify.ErrClosed):
		http.Error(w, "item already closed", http.StatusConflict)
	case errors.Is(err, notify.ErrPending):
		http.Error(w, "another answer is being applied", http.StatusConflict)
	case errors.Is(err, notify.ErrNoDecision), errors.Is(err, notify.ErrNoHandler):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		slog.Warn("inbox decision failed", "err", err)
		http.Error(w, "decision failed", http.StatusInternalServerError)
	}
}
