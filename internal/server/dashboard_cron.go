package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
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

// maxCronPromptBytesDashboard mirrors dispatch.maxCronPromptBytes (8 KiB); the
// duplicate constant avoids a circular import. Each cron tick replays this
// prompt into the CLI stdin, so large prompts amplify linearly with run count.
const maxCronPromptBytesDashboard = 8 * 1024

// maxCronIDLenDashboard bounds cron job IDs arriving via URL query parameters.
// Mirrors dispatch.maxCronIDLen (64); job IDs are 8-byte hex so anything larger
// is certainly malformed. Prevents multi-MB id query strings from being echoed
// into slog attrs on the error path.
const maxCronIDLenDashboard = 64

// maxCronScheduleBytesDashboard mirrors dispatch.maxCronScheduleBytes (256 B).
// The 64 KB MaxBytesReader body cap is body-level; a single schedule field can
// still be 63 KB of garbage and reach robfig/cron's regex parser. handlePreview
// already enforces the same 256-byte cap — apply it consistently to create +
// update so both paths treat the parser as a trusted-input boundary.
const maxCronScheduleBytesDashboard = 256

// maxCronWorkDirBytesDashboard caps the raw work_dir string before it reaches
// validateWorkspace. Even absolute paths rarely exceed 1 KiB on Linux
// (PATH_MAX is typically 4096), so 1024 is generous. Without this guard a
// multi-MB work_dir body would be echoed into slog attrs via the debug-log
// on validation failure, allowing log-flood from an authenticated attacker.
const maxCronWorkDirBytesDashboard = 1024

// validateCronWorkDir rejects work_dir strings with embedded control
// characters that would corrupt slog attribute logging (ANSI injection into
// structured logs, CR/LF line-wrapping into log pipelines). Length check
// matches prompt/schedule guards so all three fields reject the same class
// of log-injection payloads at the handler edge, before validateWorkspace
// sees them.
//
// The second pass (rune-level) also rejects Unicode bidi override /
// embedding / directional isolate characters (U+202A–U+202E, U+2066–U+2069)
// and Unicode line/paragraph separators (U+2028/U+2029) which encode as
// valid UTF-8 sequences with all bytes >= 0x20 and therefore pass the
// byte loop above. These characters can flip terminal rendering and corrupt
// log pipelines that use U+2028 as a line boundary. Matches the filter
// applied by sanitizeKeyComponent in the session package so cron fields
// and session-key fields reject the same log-injection class uniformly.
//
// R172-SEC-L1: relative paths are rejected up front so the cron edge
// boundary does not depend on validateWorkspace to fail on "." / "foo/bar"
// later. Defense-in-depth: if validateWorkspace ever loosens its IsAbs
// check (e.g. to accept workspace-relative paths for a new feature) the
// cron handler continues to enforce the stricter contract inherited from
// the scheduler worker which runs on absolute paths only.
func validateCronWorkDir(wd string) error {
	if len(wd) > maxCronWorkDirBytesDashboard {
		return fmt.Errorf("work_dir exceeds %d-byte limit", maxCronWorkDirBytesDashboard)
	}
	// R179-GO-P1: validate UTF-8 before the rune-range loop below. A `for _, r
	// := range s` over broken UTF-8 silently produces utf8.RuneError (U+FFFD)
	// for each invalid byte, which IsLogInjectionRune does not flag — this lets
	// a crafted string with lone continuation bytes smuggle arbitrary bytes
	// into cron_jobs.json / WS broadcasts. Mirrors validateProjectName.
	if !utf8.ValidString(wd) {
		return fmt.Errorf("work_dir contains invalid characters")
	}
	for i := 0; i < len(wd); i++ {
		c := wd[i]
		if c == 0 || c < 0x20 || c == 0x7f {
			return fmt.Errorf("work_dir contains invalid control characters")
		}
	}
	for _, r := range wd {
		if isLogInjectionRune(r) {
			return fmt.Errorf("work_dir contains invalid unicode control characters")
		}
	}
	if !filepath.IsAbs(wd) {
		return fmt.Errorf("work_dir must be an absolute path")
	}
	return nil
}

// isLogInjectionRune is a thin wrapper around osutil.IsLogInjectionRune kept
// for existing call sites (+ dashboard_cron_validate_test.go) that reference
// the package-local name. The canonical policy lives in osutil/loginject.go;
// see R172-SEC-M4.
func isLogInjectionRune(r rune) bool { return osutil.IsLogInjectionRune(r) }

// validateNotifyTarget enforces platform allowlist + chat_id size bound.
// R177-SEC-7: additionally reject C0/C1/bidi/LS/PS runes so a crafted
// chat_id cannot land log-injection bytes in persisted cron_jobs.json
// or forge structure in the /api/cron WS broadcast.
func validateNotifyTarget(platform, chatID string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	if len(chatID) > maxNotifyChatIDLen {
		return fmt.Errorf("notify_chat_id too long")
	}
	// R179-GO-P1: guard against invalid UTF-8 before the rune loop — see
	// validateCronWorkDir for the full attack rationale.
	if !utf8.ValidString(chatID) {
		return fmt.Errorf("notify_chat_id contains invalid characters")
	}
	for i := 0; i < len(chatID); i++ {
		c := chatID[i]
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("notify_chat_id contains invalid characters")
		}
	}
	for _, r := range chatID {
		if isLogInjectionRune(r) {
			return fmt.Errorf("notify_chat_id contains invalid characters")
		}
	}
	return nil
}

// validateCronScheduleChars rejects C0/C1/bidi/LS/PS runes in a cron
// schedule expression before it reaches robfig/cron's parser. robfig
// does not scrub its input, so log lines like `slog.Debug("cron
// preview parse failed", "err", err)` would forward unescaped bidi
// overrides into operator logs. Authenticated-only endpoint so the
// CVSS is low, but this keeps the log-injection posture consistent
// across every user-controlled string entering scheduler paths.
// R177-SEC-9.
func validateCronScheduleChars(schedule string) error {
	// R179-GO-P1: UTF-8 guard before the rune loop below.
	if !utf8.ValidString(schedule) {
		return fmt.Errorf("schedule contains invalid characters")
	}
	for i := 0; i < len(schedule); i++ {
		c := schedule[i]
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("schedule contains invalid characters")
		}
	}
	for _, r := range schedule {
		if isLogInjectionRune(r) {
			return fmt.Errorf("schedule contains invalid characters")
		}
	}
	return nil
}

// validateCronPrompt rejects prompts larger than the dashboard cap or
// containing control characters. Cron prompts are delivered via stdin as a
// stream-json user message (cron/scheduler.go → session.Send → NewUserMessage),
// where json.Marshal escapes embedded \n so NDJSON framing stays intact. LF is
// therefore allowed to support multi-paragraph playbook prompts. CR is still
// rejected because `tail -f` / `journalctl` treat it as a carriage return that
// overwrites the current log line — a log-poisoning surface unrelated to
// framing. null bytes remain forbidden (execve silently truncates at the first
// NUL). Tab is allowed because prompts may indent examples.
//
// Unlike project_api.handleConfigPut's planner_prompt guard, cron prompts do
// not end up in argv — planner_prompt and scratch context still flow into
// `--append-system-prompt` and must stay single-line; do not copy this relaxed
// policy back to those fields without re-auditing their downstream writers.
//
// Second pass mirrors validateCronWorkDir: reject C1 controls + Unicode
// bidi / directional isolate / line separator runes that are >= 0x20 at
// the byte level and therefore bypass the ASCII loop above.
// validateCronTitle 是 Job.Title 在 handler 层的守门：单行（禁内嵌换行，
// 卡片布局不允许）、长度 256 rune、禁控制字符 + 日志注入 rune。空值合法
// （允许用户不填，UI 自动 fallback 到 Prompt 首行）。
// 与 validateCronPrompt 一致的清洗集，只多禁换行。
func validateCronTitle(title string) error {
	if title == "" {
		return nil
	}
	if n := utf8.RuneCountInString(title); n > cron.MaxCronTitleLen {
		return fmt.Errorf("title exceeds %d-rune limit", cron.MaxCronTitleLen)
	}
	if !utf8.ValidString(title) {
		return fmt.Errorf("title contains invalid characters")
	}
	for _, r := range title {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("title must be a single line")
		}
		if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			return fmt.Errorf("title contains invalid control characters")
		}
		if isLogInjectionRune(r) {
			return fmt.Errorf("title contains invalid unicode control characters")
		}
	}
	return nil
}

func validateCronPrompt(prompt string) error {
	if len(prompt) > maxCronPromptBytesDashboard {
		return fmt.Errorf("prompt exceeds %d-byte limit", maxCronPromptBytesDashboard)
	}
	// R179-GO-P1: UTF-8 guard before the rune loop — see validateCronWorkDir.
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("prompt contains invalid characters")
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		if c == 0 || (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			return fmt.Errorf("prompt contains invalid control characters")
		}
	}
	for _, r := range prompt {
		if isLogInjectionRune(r) {
			return fmt.Errorf("prompt contains invalid unicode control characters")
		}
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
		Title          string `json:"title,omitempty"`
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
		// Missed / MissedSince: cron-v2-polish §3.3 Increment C。
		// missed=true 表示进程休眠 / 重启空窗期该 job 错过了至少一次调度。
		// MissedSince 是"按 schedule 算上一次应跑的毫秒时刻"，UI 可以用来
		// 显示 "上次应跑于 …"。未 missed 时两个字段都省略。
		Missed      bool  `json:"missed,omitempty"`
		MissedSince int64 `json:"missed_since,omitempty"`
	}
	views := make([]cronJobView, 0, len(jobs))
	for _, entry := range jobs {
		j := entry.Job
		v := cronJobView{
			ID:             j.ID,
			Schedule:       j.Schedule,
			Prompt:         j.Prompt,
			Title:          j.Title,
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
		// missed-schedule 检测：cron-v2-polish §3.3 Increment C。
		// 只对非 paused 的 job 判定——paused 的任务用户主动停了，错过
		// 是预期行为不应告警。
		if !j.Paused {
			if missed, prevAt := cron.HasMissedSchedule(&j, time.Now(), h.scheduler.StartedAt()); missed {
				v.Missed = true
				v.MissedSince = prevAt.UnixMilli()
			}
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
		Title          string `json:"title,omitempty"`
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
	if err := validateCronTitle(req.Title); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Cap schedule length before handing to validateSchedule → robfig/cron
	// parser. MaxBytesReader caps the whole body at 64 KB, but within that
	// envelope a single 63 KB schedule field would still reach the parser
	// and force per-field regex work. Mirrors handlePreview (line 381).
	if len(req.Schedule) > maxCronScheduleBytesDashboard {
		http.Error(w, "schedule too long", http.StatusBadRequest)
		return
	}
	if err := validateCronScheduleChars(req.Schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCronPrompt(req.Prompt); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate work_dir if provided: must be under allowedRoot. Matches the
	// 403 Forbidden used by /api/sessions/send so clients see a uniform
	// status code for boundary violations rather than ambiguous 400s.
	if req.WorkDir != "" {
		if err := validateCronWorkDir(req.WorkDir); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
		Title:          req.Title,
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
		// ErrPersistFailed signals the job was inserted into the in-memory
		// map and cron scheduler but JSON marshal (and therefore the on-disk
		// store) failed; surface it as 500 so operators see the persistence
		// gap instead of the dashboard silently treating the create as a
		// successful 2xx that won't survive a restart. R51-QUAL-001.
		if errors.Is(err, cron.ErrPersistFailed) {
			slog.Error("cron AddJob persisted in-memory but store write failed", "err", err, "id", job.ID)
			http.Error(w, "job created but not persisted; please check server logs", http.StatusInternalServerError)
			return
		}
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
	// Reject obviously-oversized ids before reaching the scheduler so slog
	// attrs in the error path aren't dragged up to multi-MB strings.
	// maxCronIDLen (64) matches the IM-side guard in dispatch/commands.go.
	if len(id) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}

	j, err := h.scheduler.DeleteJobByID(id)
	if err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrPersistFailed):
			// In-memory + cron entry deletion already happened, but the
			// store write failed — a restart would replay the deleted job.
			// 500 alerts the operator to inspect logs instead of treating
			// the delete as quietly successful. R51-QUAL-001.
			slog.Error("cron DeleteJobByID deletion not persisted", "err", err, "id", id)
			http.Error(w, "job deleted but not persisted; please check server logs", http.StatusInternalServerError)
		default:
			slog.Debug("cron delete failed", "err", err)
			http.Error(w, "delete failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job deleted via dashboard", "id", j.ID)
	writeOK(w)
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
	// Mirror handleDelete's guard so oversized IDs don't drag slog attrs up
	// to KB-scale strings on failure/success paths. R64-SEC-1.
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}

	if _, err := h.scheduler.PauseJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobAlreadyPaused):
			http.Error(w, "job already paused", http.StatusConflict)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron PauseJobByID pause not persisted", "err", err, "id", req.ID)
			http.Error(w, "job paused but not persisted; please check server logs", http.StatusInternalServerError)
		default:
			slog.Debug("cron pause failed", "err", err)
			http.Error(w, "pause failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job paused via dashboard", "id", req.ID)
	writeOK(w)
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
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}

	if _, err := h.scheduler.ResumeJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobNotPaused):
			http.Error(w, "job not paused", http.StatusConflict)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron ResumeJobByID resume not persisted", "err", err, "id", req.ID)
			http.Error(w, "job resumed but not persisted; please check server logs", http.StatusInternalServerError)
		default:
			slog.Debug("cron resume failed", "err", err)
			http.Error(w, "resume failed", http.StatusBadRequest)
		}
		return
	}

	slog.Info("cron job resumed via dashboard", "id", req.ID)
	writeOK(w)
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
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
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
	if err := validateCronScheduleChars(schedule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		// Don't echo the raw robfig/cron parser error: it leaks field offsets
		// and internal token names that help an attacker enumerate accepted
		// grammar. Log the detail for operators instead.
		slog.Debug("cron preview parse failed", "err", err)
		writeJSON(w, map[string]any{"valid": false, "error": "invalid schedule expression"})
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
	if len(id) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
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
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == nil && req.Prompt == nil && req.Title == nil && req.WorkDir == nil &&
		req.Notify == nil && req.NotifyPlatform == nil && req.NotifyChatID == nil &&
		req.FreshContext == nil {
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

	// Re-validate workspace against allowedRoot; a cleared WorkDir is
	// accepted as-is and will fall back to the router default. 403 matches
	// handleCreate and the send handler for boundary violations.
	if req.WorkDir != nil && *req.WorkDir != "" {
		if err := validateCronWorkDir(*req.WorkDir); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
		Title:          req.Title,
		WorkDir:        req.WorkDir,
		Notify:         req.Notify,
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
		FreshContext:   req.FreshContext,
	})
	if err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			// Fixed string (not err.Error()) to stay consistent with
			// handleDelete and guard against future ErrJobNotFound variants
			// that carry a wrapped ID.
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrPersistFailed):
			slog.Error("cron UpdateJob update not persisted", "err", err, "id", id)
			http.Error(w, "job updated but not persisted; please check server logs", http.StatusInternalServerError)
		default:
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
