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

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// HandleUpdate is the PATCH /api/cron endpoint. See dashboard_cron.go for the
// shared validateCron* helpers and cronUpdateResp wire shape.
func (h *Handlers) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if len(id) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	// Use pointers so the caller can distinguish "leave as-is" from "clear".
	// Sending "work_dir": "" explicitly clears the override; omitting the key
	// leaves the existing value alone.
	var req struct {
		Schedule       *string `json:"schedule,omitempty"`
		Prompt         *string `json:"prompt,omitempty"`
		Title          *string `json:"title,omitempty"`
		WorkDir        *string `json:"work_dir,omitempty"`
		Notify         *bool   `json:"notify,omitempty"`
		NotifyPlatform *string `json:"notify_platform,omitempty"`
		NotifyChatID   *string `json:"notify_chat_id,omitempty"`
		FreshContext   *bool   `json:"fresh_context,omitempty"`
		// Backend pointer keeps "" semantics distinct from "leave alone":
		// nil omits, pointer-to-"" clears the override (router default),
		// pointer to a non-empty string sets it.
		Backend *string `json:"backend,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == nil && req.Prompt == nil && req.Title == nil && req.WorkDir == nil &&
		req.Notify == nil && req.NotifyPlatform == nil && req.NotifyChatID == nil &&
		req.FreshContext == nil && req.Backend == nil {
		http.Error(w, "at least one field must be provided", http.StatusBadRequest)
		return
	}
	if req.Prompt != nil {
		if err := validateCronPrompt(*req.Prompt); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Title != nil {
		if err := validateCronTitle(*req.Title); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Schedule != nil && len(*req.Schedule) > maxCronScheduleBytesDashboard {
		http.Error(w, "schedule too long", http.StatusBadRequest)
		return
	}
	if req.Schedule != nil {
		if err := validateCronScheduleChars(*req.Schedule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Backend != nil {
		if err := ValidateCronBackend(*req.Backend); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Re-validate workspace against allowedRoot; a cleared WorkDir is
	// accepted as-is and will fall back to the router default. 403 matches
	// HandleCreate and the send handler for boundary violations.
	if req.WorkDir != nil && *req.WorkDir != "" {
		if err := validateCronWorkDir(*req.WorkDir); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if h.validateWS == nil {
			http.Error(w, "cron work_dir validation not wired", http.StatusInternalServerError)
			return
		}
		validated, err := h.validateWS(*req.WorkDir, h.allowedRoot)
		if err != nil {
			status, msg := h.classifyWSErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, msg, status)
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
			http.Error(w, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id", http.StatusBadRequest)
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
			http.Error(w, "notify_platform and notify_chat_id must be patched together", http.StatusUnprocessableEntity)
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
			http.Error(w, "notify_platform and notify_chat_id must be set together", http.StatusBadRequest)
			return
		}
		if err := validateNotifyTarget(p, c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	j, err := h.scheduler.UpdateJob(id, cronpkg.JobUpdate{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Title:          req.Title,
		WorkDir:        req.WorkDir,
		Notify:         req.Notify,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		FreshContext:   req.FreshContext,
		Backend:        req.Backend,
	})
	if err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			// Fixed string (not err.Error()) to stay consistent with
			// HandleDelete and guard against future ErrJobNotFound variants
			// that carry a wrapped ID.
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron UpdateJob update not persisted", "err", err, "id", id)
			httpErrPersistFailed(w, "updated")
		default:
			// Sanitize: the underlying parser error can leak internal field
			// names and offsets if the new schedule is rejected.
			slog.Warn("cron UpdateJob rejected", "err", err, "id", id)
			http.Error(w, "invalid update payload", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job updated via dashboard", "id", j.ID)
	httputil.WriteJSON(w, cronUpdateResp{Status: "ok", ID: j.ID})
}
