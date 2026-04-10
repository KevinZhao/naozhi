package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/cron"
)

// CronHandlers groups the cron job management API endpoints.
type CronHandlers struct {
	scheduler   *cron.Scheduler
	allowedRoot string
}

// GET /api/cron — list all cron jobs (unscoped, admin view).
func (h *CronHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"jobs": []any{}})
		return
	}

	jobs := h.scheduler.ListAllJobs()
	type cronJobView struct {
		ID             string `json:"id"`
		Schedule       string `json:"schedule"`
		Prompt         string `json:"prompt"`
		Platform       string `json:"platform"`
		ChatID         string `json:"chat_id"`
		CreatedBy      string `json:"created_by,omitempty"`
		CreatedAt      int64  `json:"created_at"`
		Paused         bool   `json:"paused"`
		WorkDir        string `json:"work_dir,omitempty"`
		NotifyPlatform string `json:"notify_platform,omitempty"`
		NotifyChatID   string `json:"notify_chat_id,omitempty"`
		LastResult     string `json:"last_result,omitempty"`
		LastRunAt      int64  `json:"last_run_at,omitempty"`
		LastError      string `json:"last_error,omitempty"`
		NextRun        int64  `json:"next_run,omitempty"`
	}
	views := make([]cronJobView, 0, len(jobs))
	for _, j := range jobs {
		v := cronJobView{
			ID:             j.ID,
			Schedule:       j.Schedule,
			Prompt:         j.Prompt,
			Platform:       j.Platform,
			ChatID:         j.ChatID,
			CreatedBy:      j.CreatedBy,
			CreatedAt:      j.CreatedAt.UnixMilli(),
			Paused:         j.Paused,
			WorkDir:        j.WorkDir,
			NotifyPlatform: j.NotifyPlatform,
			NotifyChatID:   j.NotifyChatID,
			LastResult:     j.LastResult,
			LastError:      j.LastError,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if next := h.scheduler.NextRunByID(j.ID); !next.IsZero() {
			v.NextRun = next.UnixMilli()
		}
		views = append(views, v)
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"jobs": views})
}

// POST /api/cron — create a new cron job from dashboard.
func (h *CronHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		Schedule       string `json:"schedule"`
		Prompt         string `json:"prompt"`
		WorkDir        string `json:"work_dir,omitempty"`
		NotifyPlatform string `json:"notify_platform,omitempty"`
		NotifyChatID   string `json:"notify_chat_id,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64 KB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == "" {
		http.Error(w, "schedule is required", http.StatusBadRequest)
		return
	}

	// Validate work_dir if provided: must be under allowedRoot.
	if req.WorkDir != "" {
		validated, err := validateWorkspace(req.WorkDir, h.allowedRoot)
		if err != nil {
			http.Error(w, "work_dir: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.WorkDir = validated
	}

	job := &cron.Job{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Platform:       "dashboard",
		ChatID:         "global",
		CreatedBy:      "dashboard",
		WorkDir:        req.WorkDir,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		Paused:         req.Prompt == "", // auto-pause when no prompt
	}
	if err := h.scheduler.AddJob(job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("cron job created via dashboard", "id", job.ID, "schedule", job.Schedule)
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"id": job.ID})
}

// DELETE /api/cron?id=xxx — delete a cron job by exact ID.
func (h *CronHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	j, err := h.scheduler.DeleteJobByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	slog.Info("cron job deleted via dashboard", "id", j.ID)
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"status": "ok"})
}

// POST /api/cron/pause — pause a cron job by exact ID.
func (h *CronHandlers) handlePause(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	if _, err := h.scheduler.PauseJobByID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("cron job paused via dashboard", "id", req.ID)
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"status": "ok"})
}

// POST /api/cron/resume — resume a paused cron job by exact ID.
func (h *CronHandlers) handleResume(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	if _, err := h.scheduler.ResumeJobByID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("cron job resumed via dashboard", "id", req.ID)
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"status": "ok"})
}

// GET /api/cron/preview?schedule=... — validate schedule and return next run time.
func (h *CronHandlers) handlePreview(w http.ResponseWriter, r *http.Request) {
	schedule := r.URL.Query().Get("schedule")
	if schedule == "" {
		http.Error(w, "schedule is required", http.StatusBadRequest)
		return
	}

	next, err := cron.PreviewSchedule(schedule)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"valid": false, "error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"valid": true, "next_run": next.UnixMilli()})
}
