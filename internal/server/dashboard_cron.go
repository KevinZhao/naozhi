package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
)

// Bounds for notify target fields set by authenticated dashboard users. The
// platform must match a known IM provider to avoid silent notification drops
// (misspelt names used to fall through); chat_id length is capped so a user
// cannot stuff megabytes into cron_jobs.json via a single API call.
var validNotifyPlatforms = map[string]struct{}{
	"":        {}, // empty = fall back to cron.notify_default
	"feishu":  {},
	"slack":   {},
	"discord": {},
	"weixin":  {},
}

const maxNotifyChatIDLen = 256

// validateNotifyTarget enforces platform allowlist + chat_id size bound.
func validateNotifyTarget(platform, chatID string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	if len(chatID) > maxNotifyChatIDLen {
		return fmt.Errorf("notify_chat_id too long")
	}
	return nil
}

// CronHandlers groups the cron job management API endpoints.
type CronHandlers struct {
	scheduler   *cron.Scheduler
	allowedRoot string
}

// GET /api/cron — list all cron jobs (unscoped, admin view).
func (h *CronHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		writeJSON(w, map[string]any{"jobs": []any{}})
		return
	}

	jobs := h.scheduler.ListAllJobsWithNextRun()
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
		// Notify is a pointer so the view preserves the tri-state (nil vs
		// explicit true/false). nil renders as "legacy default" on the client.
		Notify       *bool `json:"notify,omitempty"`
		FreshContext bool  `json:"fresh_context,omitempty"`
	}
	views := make([]cronJobView, 0, len(jobs))
	for _, entry := range jobs {
		j := entry.Job
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
			Notify:         j.Notify,
			FreshContext:   j.FreshContext,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if !entry.NextRun.IsZero() {
			v.NextRun = entry.NextRun.UnixMilli()
		}
		views = append(views, v)
	}

	loc := h.scheduler.Location()
	name, offset := time.Now().In(loc).Zone()
	tzLabel := formatTZOffset(loc.String(), offset)

	resp := map[string]any{
		"jobs":           views,
		"timezone":       loc.String(),
		"timezone_label": tzLabel,
		"timezone_abbr":  name,
	}
	if def := h.scheduler.NotifyDefault(); def.IsSet() {
		// Expose the configured default so the UI can render helpful copy
		// like "notifications go to feishu (oc_xxx)" instead of just a
		// blank toggle. chat_id is already considered semi-public (appears
		// in message metadata) so surfacing it here is not a leak.
		resp["notify_default"] = map[string]string{
			"platform": def.Platform,
			"chat_id":  def.ChatID,
		}
	}
	writeJSON(w, resp)
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
		Notify         *bool  `json:"notify,omitempty"`
		FreshContext   bool   `json:"fresh_context,omitempty"`
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

	// Validate work_dir if provided: must be under allowedRoot. Matches the
	// 403 Forbidden used by /api/sessions/send so clients see a uniform
	// status code for boundary violations rather than ambiguous 400s.
	if req.WorkDir != "" {
		validated, err := validateWorkspace(req.WorkDir, h.allowedRoot)
		if err != nil {
			// Avoid echoing the raw validation detail (which can reveal the
			// allowedRoot boundary or path shape); operators can diagnose from
			// server logs if needed.
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, "invalid work_dir", http.StatusForbidden)
			return
		}
		req.WorkDir = validated
	}

	// Guard: notify=true without any target (neither per-job override nor
	// scheduler default) would silently swallow notifications. Reject it
	// at the edge so users see the problem immediately.
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != "" && req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			http.Error(w, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id", http.StatusBadRequest)
			return
		}
	}

	if err := validateNotifyTarget(req.NotifyPlatform, req.NotifyChatID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
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
		Notify:         req.Notify,
		FreshContext:   req.FreshContext,
		Paused:         req.Prompt == "", // auto-pause when no prompt
	}
	if err := h.scheduler.AddJob(job); err != nil {
		// robfig/cron parser errors can mention internal field offsets and
		// parsed expressions; log the full detail for operator triage but
		// return a sanitized message to the dashboard client.
		slog.Warn("cron AddJob rejected", "err", err, "schedule", job.Schedule)
		http.Error(w, "invalid schedule or job fields", http.StatusBadRequest)
		return
	}

	slog.Info("cron job created via dashboard", "id", job.ID, "schedule", job.Schedule)
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
		if errors.Is(err, cron.ErrJobNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
		} else {
			slog.Debug("cron delete failed", "err", err)
			http.Error(w, "delete failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job deleted via dashboard", "id", j.ID)
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
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobAlreadyPaused):
			http.Error(w, "job already paused", http.StatusConflict)
		default:
			slog.Debug("cron pause failed", "err", err)
			http.Error(w, "pause failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job paused via dashboard", "id", req.ID)
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
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobNotPaused):
			http.Error(w, "job not paused", http.StatusConflict)
		default:
			slog.Debug("cron resume failed", "err", err)
			http.Error(w, "resume failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job resumed via dashboard", "id", req.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

// POST /api/cron/trigger — manually trigger a cron job execution (for debugging).
func (h *CronHandlers) handleTrigger(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	if err := h.scheduler.TriggerNow(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobPaused):
			http.Error(w, "job is paused", http.StatusConflict)
		default:
			slog.Debug("cron trigger failed", "err", err)
			http.Error(w, "trigger failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job triggered manually", "id", req.ID)
	writeJSON(w, map[string]string{"status": "triggered"})
}

// GET /api/cron/preview?schedule=...&count=N — validate schedule and return
// the next N run times. count defaults to 1 and is clamped to [1, 10] so the
// UI can show a multi-run preview without giving callers an unbounded knob.
func (h *CronHandlers) handlePreview(w http.ResponseWriter, r *http.Request) {
	schedule := r.URL.Query().Get("schedule")
	if schedule == "" {
		http.Error(w, "schedule is required", http.StatusBadRequest)
		return
	}
	// Cap schedule length so the cron parser (regex + split) cannot be DoS'd
	// with a megabyte-scale query parameter. Real cron expressions are far
	// below this limit; robfig/cron rejects extremely long descriptors anyway.
	if len(schedule) > 256 {
		http.Error(w, "schedule too long", http.StatusBadRequest)
		return
	}

	count := 1
	if raw := r.URL.Query().Get("count"); raw != "" {
		// Reject obviously huge inputs before Atoi so an attacker cannot force
		// us to decode a multi-kilobyte digit string.
		if len(raw) > 3 {
			http.Error(w, "count must be a positive integer", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "count must be a positive integer", http.StatusBadRequest)
			return
		}
		if n > 10 {
			n = 10
		}
		count = n
	}

	var (
		runs    []time.Time
		err     error
		tzName  = "UTC"
		tzLabel = ""
	)
	if h.scheduler != nil {
		runs, err = h.scheduler.PreviewScheduleN(schedule, count)
		loc := h.scheduler.Location()
		tzName = loc.String()
		if n, offset := time.Now().In(loc).Zone(); n != "" {
			tzLabel = formatTZOffset(tzName, offset)
		}
	} else {
		// Fallback for tests/bootstrap where scheduler isn't wired: compute in UTC.
		var next time.Time
		next, err = cron.PreviewSchedule(schedule)
		if err == nil {
			runs = []time.Time{next}
		}
	}
	if err != nil {
		writeJSON(w, map[string]any{"valid": false, "error": err.Error()})
		return
	}

	resp := map[string]any{
		"valid":    true,
		"timezone": tzName,
	}
	if tzLabel != "" {
		resp["timezone_label"] = tzLabel
	}
	if len(runs) > 0 {
		resp["next_run"] = runs[0].UnixMilli()
		nextRuns := make([]int64, len(runs))
		for i, t := range runs {
			nextRuns[i] = t.UnixMilli()
		}
		resp["next_runs"] = nextRuns
	}
	writeJSON(w, resp)
}

// PATCH /api/cron?id=xxx — edit schedule / prompt / work_dir on an existing job.
func (h *CronHandlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	// Use pointers so the caller can distinguish "leave as-is" from "clear".
	// Sending "work_dir": "" explicitly clears the override; omitting the key
	// leaves the existing value alone.
	var req struct {
		Schedule       *string `json:"schedule,omitempty"`
		Prompt         *string `json:"prompt,omitempty"`
		WorkDir        *string `json:"work_dir,omitempty"`
		Notify         *bool   `json:"notify,omitempty"`
		NotifyPlatform *string `json:"notify_platform,omitempty"`
		NotifyChatID   *string `json:"notify_chat_id,omitempty"`
		FreshContext   *bool   `json:"fresh_context,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == nil && req.Prompt == nil && req.WorkDir == nil &&
		req.Notify == nil && req.NotifyPlatform == nil && req.NotifyChatID == nil &&
		req.FreshContext == nil {
		http.Error(w, "at least one field must be provided", http.StatusBadRequest)
		return
	}

	// Re-validate workspace against allowedRoot; a cleared WorkDir is
	// accepted as-is and will fall back to the router default. 403 matches
	// handleCreate and the send handler for boundary violations.
	if req.WorkDir != nil && *req.WorkDir != "" {
		validated, err := validateWorkspace(*req.WorkDir, h.allowedRoot)
		if err != nil {
			// Avoid echoing the raw validation detail (which can reveal the
			// allowedRoot boundary or path shape); operators can diagnose from
			// server logs if needed.
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, "invalid work_dir", http.StatusForbidden)
			return
		}
		req.WorkDir = &validated
	}

	// Guard: notify=true with no effective target would silently drop
	// notifications. Mirror the handleCreate check.
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != nil && *req.NotifyPlatform != "" &&
			req.NotifyChatID != nil && *req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			http.Error(w, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id", http.StatusBadRequest)
			return
		}
	}

	// Validate notify target only when the caller is actually changing it.
	if req.NotifyPlatform != nil || req.NotifyChatID != nil {
		p := ""
		if req.NotifyPlatform != nil {
			p = *req.NotifyPlatform
		}
		c := ""
		if req.NotifyChatID != nil {
			c = *req.NotifyChatID
		}
		if err := validateNotifyTarget(p, c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	j, err := h.scheduler.UpdateJob(id, cron.JobUpdate{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		WorkDir:        req.WorkDir,
		Notify:         req.Notify,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		FreshContext:   req.FreshContext,
	})
	if err != nil {
		if errors.Is(err, cron.ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			// Sanitize: the underlying parser error can leak internal field
			// names and offsets if the new schedule is rejected.
			slog.Warn("cron UpdateJob rejected", "err", err, "id", id)
			http.Error(w, "invalid update payload", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job updated via dashboard", "id", j.ID)
	writeJSON(w, map[string]any{"status": "ok", "id": j.ID})
}

// formatTZOffset renders a timezone label like "Asia/Shanghai (UTC+08:00)" or
// "America/St_Johns (UTC-03:30)". The integer-division approach would produce
// "UTC-05:-30" for fractional negative offsets because the sub-hour remainder
// inherits the sign; abs() the minute component to keep the format well-formed.
func formatTZOffset(name string, offsetSeconds int) string {
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	if minutes < 0 {
		minutes = -minutes
	}
	return fmt.Sprintf("%s (UTC%+03d:%02d)", name, hours, minutes)
}
