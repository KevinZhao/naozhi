package cron

import (
	"errors"
	"log/slog"
	"net/http"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// HandleUpdate is the PATCH /api/cron endpoint.
func (h *Handlers) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit: every call writes cron_jobs.json and mutates the
	// scheduler map (loop-PATCH disk IO amplification). Nil-guarded for tests.
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
	if len(id) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(id) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	// Pointers distinguish "leave as-is" (key omitted) from "clear" (explicit "").
	var req struct {
		Schedule *string `json:"schedule,omitempty"`
		Prompt   *string `json:"prompt,omitempty"`
		Title    *string `json:"title,omitempty"`
		WorkDir  *string `json:"work_dir,omitempty"`
		Notify   *bool   `json:"notify,omitempty"`
		// NotifyClear: pointer-to-true resets Job.Notify to legacy-default (#958).
		NotifyClear    *bool   `json:"notify_clear,omitempty"`
		NotifyPlatform *string `json:"notify_platform,omitempty"`
		NotifyChatID   *string `json:"notify_chat_id,omitempty"`
		FreshContext   *bool   `json:"fresh_context,omitempty"`
		// Backend: pointer-to-"" clears the override (router default).
		Backend *string `json:"backend,omitempty"`
		// Placement: ""/"local" 本机；"sandbox" 云沙箱 (RFC §4.2)。
		Placement *string `json:"placement,omitempty"`
		// SideEffects: pointer 写显式三态（agentcore §6.2）。
		SideEffects *bool `json:"side_effects,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeCronErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Schedule == nil && req.Prompt == nil && req.Title == nil && req.WorkDir == nil &&
		req.Notify == nil && req.NotifyClear == nil && req.NotifyPlatform == nil && req.NotifyChatID == nil &&
		req.FreshContext == nil && req.Backend == nil && req.Placement == nil && req.SideEffects == nil {
		writeCronErr(w, http.StatusBadRequest, "at least one field must be provided")
		return
	}
	if req.Prompt != nil {
		if err := validateCronPrompt(*req.Prompt); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Title != nil {
		if err := validateCronTitle(*req.Title); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Schedule != nil && len(*req.Schedule) > maxCronScheduleBytesDashboard {
		writeCronErr(w, http.StatusBadRequest, "schedule too long")
		return
	}
	if req.Schedule != nil {
		if err := validateCronScheduleChars(*req.Schedule); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Backend != nil {
		if err := ValidateCronBackend(*req.Backend); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Placement != nil {
		// Shape gate only: the sandbox guardrail (no work_dir) needs the EFFECTIVE
		// post-patch values, so it lives in Scheduler.UpdateJob's critical section
		// and surfaces here as ErrSandboxWorkDir.
		if err := validateCronPlacement(*req.Placement, ""); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// A cleared WorkDir falls back to the router default; 403 matches
	// HandleCreate for boundary violations.
	if req.WorkDir != nil && *req.WorkDir != "" {
		if err := validateCronWorkDir(*req.WorkDir); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if h.validateWS == nil {
			writeCronErr(w, http.StatusInternalServerError, "cron work_dir validation not wired")
			return
		}
		validated, err := h.validateWS(*req.WorkDir, h.allowedRoot)
		if err != nil {
			status, msg := h.classifyWSErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			writeCronErr(w, status, msg)
			return
		}
		req.WorkDir = &validated
	}

	// notify=true with no effective target would silently drop notifications.
	// Check the EFFECTIVE post-patch target: absent fields inherit the job's
	// current value, present fields (including explicit "") override it.
	if req.Notify != nil && *req.Notify {
		effPlatform, effChatID := "", ""
		if cur, ok := h.scheduler.GetJob(id); ok {
			effPlatform, effChatID = cur.NotifyPlatform, cur.NotifyChatID
		}
		if req.NotifyPlatform != nil {
			effPlatform = *req.NotifyPlatform
		}
		if req.NotifyChatID != nil {
			effChatID = *req.NotifyChatID
		}
		perJobSet := effPlatform != "" && effChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			writeCronErr(w, http.StatusBadRequest, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id")
			return
		}
	}

	// Validate notify target only when the caller is actually changing it.
	if req.NotifyPlatform != nil || req.NotifyChatID != nil {
		// Both notify pointers must travel together: clearing one half via ""
		// while omitting the other would leave an orphan target on disk and
		// silently re-route notifications to cron.notify_default. 422: the JSON
		// is well-formed but describes an unprocessable on-disk transition.
		if (req.NotifyPlatform == nil) != (req.NotifyChatID == nil) {
			writeCronErr(w, http.StatusUnprocessableEntity, "notify_platform and notify_chat_id must be patched together")
			return
		}
		p := ""
		if req.NotifyPlatform != nil {
			p = *req.NotifyPlatform
		}
		c := ""
		if req.NotifyChatID != nil {
			c = *req.NotifyChatID
		}
		// Disk shape must be both empty or both set; a half-set pair misroutes.
		platformSet := p != ""
		chatIDSet := c != ""
		if platformSet != chatIDSet {
			writeCronErr(w, http.StatusBadRequest, "notify_platform and notify_chat_id must be set together")
			return
		}
		if err := validateNotifyTarget(p, c); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	j, err := h.scheduler.UpdateJob(id, cronpkg.JobUpdate{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Title:          req.Title,
		WorkDir:        req.WorkDir,
		Notify:         req.Notify,
		NotifyClear:    req.NotifyClear,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		FreshContext:   req.FreshContext,
		Backend:        req.Backend,
		Placement:      req.Placement,
		SideEffects:    req.SideEffects,
	})
	if err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			// Fixed string (not err.Error()) so a wrapped ID never leaks.
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron UpdateJob update not persisted", "err", err, "id", osutil.SanitizeForLog(id, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "updated")
		case errors.Is(err, cronpkg.ErrSandboxWorkDir):
			// Effective placement×work_dir rejected inside UpdateJob.
			writeCronErr(w, http.StatusBadRequest, "云沙箱暂不支持工作目录（Phase 1）：请先清空 work_dir 或改用本机运行")
		default:
			// Parser errors can leak internal field names / offsets; sanitize.
			slog.Warn("cron UpdateJob rejected", "err", err, "id", osutil.SanitizeForLog(id, cronpkg.MaxIDLen))
			writeCronErr(w, http.StatusBadRequest, "invalid update payload")
		}
		return
	}

	slog.Info("cron job updated via dashboard", "id", osutil.SanitizeForLog(j.ID, cronpkg.MaxIDLen))
	httputil.WriteJSON(w, cronUpdateResp{Status: "ok", ID: j.ID})
}
