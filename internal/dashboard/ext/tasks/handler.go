// Package tasks hosts the "任务" endpoints over internal/planner/task
// (docs/rfc/planner.md §10.1, §15.4):
//
//	GET  /api/tasks        the dispatched sessions and the promises not archived, by section
//	POST /api/tasks/state  set a task's state ({"id","state":"doing"|"done"}) as the user
//	POST /api/tasks/stop   stop a dispatched session ({"id"}): its process, and 已完成 (已停止) as the user's
//
// All sit behind the /api/* auth middleware: a request here is the user.
package tasks

import (
	"errors"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/planner/task"
)

// Source is the task list (task.Board).
type Source interface {
	List() []task.Task
	Set(id string, state task.State, by task.By, why string) error
	Stop(id string) error
}

// Handler serves the endpoints. A nil source lists nothing and refuses
// changes with 503.
type Handler struct {
	src Source
}

// New returns a Handler over src.
func New(src Source) *Handler { return &Handler{src: src} }

type listResp struct {
	Tasks []task.Task `json:"tasks"`
}

// HandleList serves GET /api/tasks.
func (h *Handler) HandleList(w http.ResponseWriter, _ *http.Request) {
	resp := listResp{Tasks: []task.Task{}}
	if h.src != nil {
		if l := h.src.List(); l != nil {
			resp.Tasks = l
		}
	}
	httputil.WriteJSON(w, resp)
}

// HandleState serves POST /api/tasks/state: the user's state menu offers
// 进行中 (reopening a done task) and 已完成.
func (h *Handler) HandleState(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		ID    string     `json:"id"`
		State task.State `json:"state"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.State != task.Doing && req.State != task.Done {
		http.Error(w, "state must be doing or done", http.StatusBadRequest)
		return
	}
	if h.src == nil {
		http.Error(w, "tasks not available", http.StatusServiceUnavailable)
		return
	}
	reply(w, h.src.Set(req.ID, req.State, task.ByUser, ""))
}

// HandleStop serves POST /api/tasks/stop: the 停止 of a dispatched session.
func (h *Handler) HandleStop(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	var req struct {
		ID string `json:"id"`
	}
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if h.src == nil {
		http.Error(w, "tasks not available", http.StatusServiceUnavailable)
		return
	}
	reply(w, h.src.Stop(req.ID))
}

func reply(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		httputil.WriteOK(w)
	case errors.Is(err, task.ErrNotFound):
		http.Error(w, "no such task", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
