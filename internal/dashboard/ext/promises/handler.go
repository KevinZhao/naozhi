// Package promises hosts the "答应你的" endpoints over
// internal/planner/promise (docs/rfc/planner.md §3.6, §10.1):
//
//	GET  /api/promises        open promises, earliest due first, and the recently closed
//	POST /api/promises/close  close an open promise ({"id"}); only the user closes one
//
// Both sit behind the /api/* auth middleware: a request here is the user.
package promises

import (
	"errors"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/promise"
)

// recentClosed is how many closed promises GET /api/promises lists.
const recentClosed = 50

// Source is the promise store (promise.Store).
type Source interface {
	List(limit int) (open, closed []promise.Promise)
	Close(id string, by promise.Source) error
}

// Handler serves the endpoints. A nil source lists nothing and refuses
// changes with 503.
type Handler struct {
	src Source
}

// New returns a Handler over src.
func New(src Source) *Handler { return &Handler{src: src} }

type listResp struct {
	Open   []promise.Promise `json:"open"`
	Closed []promise.Promise `json:"closed"`
}

// HandleList serves GET /api/promises.
func (h *Handler) HandleList(w http.ResponseWriter, _ *http.Request) {
	resp := listResp{Open: []promise.Promise{}, Closed: []promise.Promise{}}
	if h.src != nil {
		open, closed := h.src.List(recentClosed)
		if open != nil {
			resp.Open = open
		}
		if closed != nil {
			resp.Closed = closed
		}
	}
	httputil.WriteJSON(w, resp)
}

// HandleClose serves POST /api/promises/close.
func (h *Handler) HandleClose(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		ID string `json:"id"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if h.src == nil {
		http.Error(w, "promises not available", http.StatusServiceUnavailable)
		return
	}
	switch err := h.src.Close(req.ID, promise.SourceUser); {
	case err == nil:
		httputil.WriteOK(w)
	case errors.Is(err, promise.ErrNotFound):
		http.Error(w, "no such promise", http.StatusNotFound)
	case errors.Is(err, promise.ErrClosed):
		http.Error(w, "promise already closed", http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
