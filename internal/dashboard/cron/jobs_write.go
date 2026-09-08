package cron

import (
	"errors"
	"log/slog"
	"net/http"

	cronpkg "github.com/naozhi/naozhi/internal/cron"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// httpErrPersistFailed writes the 500 errResp envelope for "in-memory mutation
// succeeded but on-disk persist failed", shared by the five cron write handlers.
// Code is the stable "persist_failed" token; the message keeps the verb (#1274).
func httpErrPersistFailed(w http.ResponseWriter, op string) {
	httputil.WriteJSONStatus(w, http.StatusInternalServerError, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{
		Error: "job " + op + " but not persisted; please check server logs",
		Code:  "persist_failed",
	})
}

// writeCronErr writes a cron mutation error as the JSON envelope {"error": msg}
// so dashboard.js reads body.error uniformly (#1518).
func writeCronErr(w http.ResponseWriter, status int, msg string) {
	httputil.WriteJSONStatus(w, status, map[string]string{"error": msg})
}

// POST /api/cron — create a new cron job from dashboard.
func (h *Handlers) HandleCreate(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit: mutations write cron_jobs.json and mutate the scheduler
	// map, so a stolen token must not amplify IO damage. Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		Schedule       string `json:"schedule"`
		Prompt         string `json:"prompt"`
		Title          string `json:"title,omitempty"`
		WorkDir        string `json:"work_dir,omitempty"`
		NotifyPlatform string `json:"notify_platform,omitempty"`
		NotifyChatID   string `json:"notify_chat_id,omitempty"`
		Notify         *bool  `json:"notify,omitempty"`
		FreshContext   bool   `json:"fresh_context,omitempty"`
		// Backend pins the CLI backend ("" = router default); see ValidateCronBackend.
		Backend string `json:"backend,omitempty"`
		// Placement: ""/"local" = this host, "sandbox" = AgentCore microVM
		// (RFC §4.2); validateCronPlacement gates values and the sandbox guardrails.
		Placement string `json:"placement,omitempty"`
		// SideEffects declares external mutation (agentcore §6.2); nil = off.
		SideEffects *bool `json:"side_effects,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64 KB
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeCronErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Schedule == "" {
		writeCronErr(w, http.StatusBadRequest, "schedule is required")
		return
	}
	if err := validateCronTitle(req.Title); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cap schedule length before the robfig/cron parser: MaxBytesReader caps
	// the body at 64 KB but a 63 KB schedule would still reach the parser.
	if len(req.Schedule) > maxCronScheduleBytesDashboard {
		writeCronErr(w, http.StatusBadRequest, "schedule too long")
		return
	}
	if err := validateCronScheduleChars(req.Schedule); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCronPrompt(req.Prompt); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ValidateCronBackend(req.Backend); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCronPlacement(req.Placement, req.WorkDir); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// work_dir must be under allowedRoot; 403 matches /api/sessions/send so
	// clients see a uniform status for boundary violations.
	if req.WorkDir != "" {
		if err := validateCronWorkDir(req.WorkDir); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if h.validateWS == nil {
			writeCronErr(w, http.StatusInternalServerError, "cron work_dir validation not wired")
			return
		}
		validated, err := h.validateWS(req.WorkDir, h.allowedRoot)
		if err != nil {
			status, msg := h.classifyWSErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			writeCronErr(w, status, msg)
			return
		}
		req.WorkDir = validated
	}

	// notify=true without any target (per-job override or scheduler default)
	// would silently swallow notifications. A per-job override counts as "set"
	// only when BOTH fields are present; a half-set pair is a form-fill mistake.
	if req.NotifyPlatform != "" || req.NotifyChatID != "" {
		if req.NotifyPlatform == "" || req.NotifyChatID == "" {
			writeCronErr(w, http.StatusBadRequest, "notify_platform and notify_chat_id must be set together")
			return
		}
	}
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != "" && req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			writeCronErr(w, http.StatusBadRequest, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id")
			return
		}
	}

	if err := validateNotifyTarget(req.NotifyPlatform, req.NotifyChatID); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}

	job := &cronpkg.Job{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Title:          req.Title,
		Platform:       "dashboard",
		ChatID:         "global",
		CreatedBy:      "dashboard",
		WorkDir:        req.WorkDir,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		Notify:         req.Notify,
		FreshContext:   req.FreshContext,
		Backend:        req.Backend,
		Placement:      req.Placement,
		SideEffects:    req.SideEffects,
		Paused:         req.Prompt == "", // auto-pause when no prompt
	}
	if err := h.scheduler.AddJob(job); err != nil {
		// ErrPersistFailed: in-memory insert succeeded but the store write
		// failed; surface 500 rather than a 2xx that won't survive a restart.
		if errors.Is(err, cronpkg.ErrPersistFailed) {
			slog.Error("cron AddJob persisted in-memory but store write failed", "err", err, "id", osutil.SanitizeForLog(job.ID, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "created")
			return
		}
		// robfig/cron parser errors leak field offsets / parsed expressions;
		// log the detail for operators, return a sanitized message.
		slog.Warn("cron AddJob rejected", "err", err, "schedule", job.Schedule)
		writeCronErr(w, http.StatusBadRequest, "invalid schedule or job fields")
		return
	}

	slog.Info("cron job created via dashboard", "id", osutil.SanitizeForLog(job.ID, cronpkg.MaxIDLen), "schedule", job.Schedule)
	httputil.WriteJSON(w, cronCreateResp{ID: job.ID})
}

// DELETE /api/cron?id=xxx — delete a cron job by exact ID.
func (h *Handlers) HandleDelete(w http.ResponseWriter, r *http.Request) {
	// Per-IP write rate limit (see HandleCreate). Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	// Reject oversized ids before slog attrs in the error path inflate;
	// maxCronIDLen matches the IM-side guard in dispatch/commands.go.
	if len(id) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog (log forgery on lookup miss).
	if !cronpkg.IsValidID(id) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	j, err := h.scheduler.DeleteJobByID(id)
	if err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			// Deletion already happened in memory; a restart would replay the job.
			slog.Error("cron DeleteJobByID deletion not persisted", "err", err, "id", osutil.SanitizeForLog(id, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "deleted")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron delete failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "delete failed")
		}
		return
	}

	slog.Info("cron job deleted via dashboard", "id", osutil.SanitizeForLog(j.ID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// POST /api/cron/pause — pause a cron job by exact ID.
func (h *Handlers) HandlePause(w http.ResponseWriter, r *http.Request) {
	// Per-IP write rate limit (see HandleCreate). Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.ID == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	// Mirror HandleDelete's guard so oversized IDs don't inflate slog attrs.
	if len(req.ID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(req.ID) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	if _, err := h.scheduler.PauseJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobAlreadyPaused):
			writeCronErr(w, http.StatusConflict, "job already paused")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron PauseJobByID pause not persisted", "err", err, "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "paused")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron pause failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "pause failed")
		}
		return
	}

	slog.Info("cron job paused via dashboard", "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// POST /api/cron/resume — resume a paused cron job by exact ID.
func (h *Handlers) HandleResume(w http.ResponseWriter, r *http.Request) {
	// Per-IP write rate limit (see HandleCreate). Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.ID == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(req.ID) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	if _, err := h.scheduler.ResumeJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobNotPaused):
			writeCronErr(w, http.StatusConflict, "job not paused")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron ResumeJobByID resume not persisted", "err", err, "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "resumed")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron resume failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "resume failed")
		}
		return
	}

	slog.Info("cron job resumed via dashboard", "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// POST /api/cron/trigger — manually trigger a cron job execution (for debugging).
func (h *Handlers) HandleTrigger(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit: each call spawns the job's claude CLI subprocess and
	// may emit IM notifications — a loop-trigger amplification vector.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeCronErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ID == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(req.ID) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.scheduler.TriggerNow(req.ID); err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobPaused):
			writeCronErr(w, http.StatusConflict, "job is paused")
		case errors.Is(err, cronpkg.ErrJobNoPrompt):
			writeCronErr(w, http.StatusUnprocessableEntity, "job has no prompt")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron trigger failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "trigger failed")
		}
		return
	}

	slog.Info("cron job triggered manually", "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
	httputil.WriteJSON(w, map[string]string{"status": "triggered"})
}
