package cron

// File extracted from dashboard_cron.go (#1281) — HandleUpdate is the largest
// function in the cron HTTP surface (203 lines) and gates every PATCH /api/cron
// request through six independent validation passes (id shape, body decode,
// per-field rune scrub, work_dir workspace check, notify-target coherency,
// scheduler.UpdateJob). Extracting it gives the rest of dashboard_cron.go room
// to stay readable without changing wireup or struct shape.

import (
	"errors"
	"log/slog"
	"net/http"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// HandleUpdate is the PATCH /api/cron endpoint. See dashboard_cron.go for the
// shared validateCron* helpers and cronUpdateResp wire shape.
func (h *Handlers) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	// [R20260607-SEC-1] Per-IP rate limit: HandleUpdate writes cron_jobs.json
	// and mutates the scheduler map on every call (validateWorkspace×2 +
	// persist). A stolen dashboard token without this gate could loop-PATCH to
	// exhaust disk IO. Nil-guarded for hand-built test handlers (matches
	// HandleCreate/Delete/Pause/Resume/Trigger/Preview pattern).
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
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(id) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	// Use pointers so the caller can distinguish "leave as-is" from "clear".
	// Sending "work_dir": "" explicitly clears the override; omitting the key
	// leaves the existing value alone.
	var req struct {
		Schedule *string `json:"schedule,omitempty"`
		Prompt   *string `json:"prompt,omitempty"`
		Title    *string `json:"title,omitempty"`
		WorkDir  *string `json:"work_dir,omitempty"`
		Notify   *bool   `json:"notify,omitempty"`
		// NotifyClear wires the scheduler's reset-to-nil opt-in (JobUpdate.
		// NotifyClear, R249-CR-15 #958) onto the HTTP surface. Pointer-to-true
		// resets Job.Notify back to legacy-default (inherit scheduler policy);
		// nil or pointer-to-false is a no-op. Without this field the reset path
		// was unreachable over HTTP. [R103901-GO-1]
		NotifyClear    *bool   `json:"notify_clear,omitempty"`
		NotifyPlatform *string `json:"notify_platform,omitempty"`
		NotifyChatID   *string `json:"notify_chat_id,omitempty"`
		FreshContext   *bool   `json:"fresh_context,omitempty"`
		// Backend pointer keeps "" semantics distinct from "leave alone":
		// nil omits, pointer-to-"" clears the override (router default),
		// pointer to a non-empty string sets it.
		Backend *string `json:"backend,omitempty"`
		// Placement: nil omits; ""/"local" 本机；"sandbox" 云沙箱
		// (agentcore-cloud-sandbox RFC §4.2)。
		Placement *string `json:"placement,omitempty"`
		// SideEffects: nil omits; pointer 写显式三态（agentcore §6.2）。
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
		// Shape gate only ("", local, sandbox). The Phase 1 sandbox
		// guardrail (no work_dir) needs the EFFECTIVE post-patch values —
		// a PATCH flipping placement=sandbox on a job that already has a
		// work_dir is as invalid as setting both at once. That cross-field
		// check lives in Scheduler.UpdateJob's critical section, the only
		// place that sees the live job and the patch atomically; it
		// surfaces here as ErrSandboxWorkDir → a precise 400.
		if err := validateCronPlacement(*req.Placement, ""); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Re-validate workspace against allowedRoot; a cleared WorkDir is
	// accepted as-is and will fall back to the router default. 403 matches
	// HandleCreate and the send handler for boundary violations.
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

	// Guard: notify=true with no effective target would silently drop
	// notifications. Mirror the HandleCreate check.
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != nil && *req.NotifyPlatform != "" &&
			req.NotifyChatID != nil && *req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			writeCronErr(w, http.StatusBadRequest, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id")
			return
		}
	}

	// Validate notify target only when the caller is actually changing it.
	if req.NotifyPlatform != nil || req.NotifyChatID != nil {
		// R238-SEC-14: a PATCH that touches ONE notify field but omits the
		// other lands an orphan-target on disk. Concrete failure: the job
		// already has {platform="feishu", chat_id="oc_xxx"} and the caller
		// PATCHes notify_platform:"" without notify_chat_id — UpdateJob
		// clears NotifyPlatform but leaves NotifyChatID="oc_xxx", silently
		// re-routing notifications to the cron.notify_default fallback
		// instead of the explicit per-job target the operator just edited.
		// The platformSet/chatIDSet check below catches the (set,absent)
		// and (absent,set) cases but not (cleared-via-empty,absent) and
		// (absent,cleared-via-empty), because both halves coerce to "" and
		// the != check returns false. Force the caller to send both
		// pointers together so on-disk state always reflects a coherent
		// (both clear, both set) tuple. 422 mirrors the validation-shape
		// failure category — the request is well-formed JSON, the values
		// just describe an unprocessable on-disk transition.
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
		// R242-SEC-11: a half-set patch (one field present + non-empty,
		// the other present + empty OR absent) lands an orphan-target on
		// disk that silently routes notifications to the wrong place.
		// Disk shape we want is: both empty (no override) or both set.
		// Reject the half-set case so the caller can self-correct.
		// Patch leaves the missing pointer as nil — interpreted as
		// "leave existing", so a PATCH-of-one-field is a request to
		// edit one half: also disallowed for the same reason.
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
			// Fixed string (not err.Error()) to stay consistent with
			// HandleDelete and guard against future ErrJobNotFound variants
			// that carry a wrapped ID.
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron UpdateJob update not persisted", "err", err, "id", osutil.SanitizeForLog(id, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "updated")
		case errors.Is(err, cronpkg.ErrSandboxWorkDir):
			// Phase 1 sandbox guardrail (effective placement×work_dir
			// combination rejected inside UpdateJob's critical section).
			writeCronErr(w, http.StatusBadRequest, "云沙箱暂不支持工作目录（Phase 1）：请先清空 work_dir 或改用本机运行")
		default:
			// Sanitize: the underlying parser error can leak internal field
			// names and offsets if the new schedule is rejected.
			slog.Warn("cron UpdateJob rejected", "err", err, "id", osutil.SanitizeForLog(id, cronpkg.MaxIDLen))
			writeCronErr(w, http.StatusBadRequest, "invalid update payload")
		}
		return
	}

	slog.Info("cron job updated via dashboard", "id", osutil.SanitizeForLog(j.ID, cronpkg.MaxIDLen))
	httputil.WriteJSON(w, cronUpdateResp{Status: "ok", ID: j.ID})
}
