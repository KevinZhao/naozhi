package cron

import (
	"errors"
	"log/slog"
	"net/http"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// cronAttentionItemView is the wire shape of one §7.4 confirmation-queue card.
// Mirrors cronpkg.SandboxAttentionItem but SanitizeForLog's the operator-facing
// label (a job title persisted before the validator was tightened, or
// hand-edited on disk, could carry control/bidi runes).
type cronAttentionItemView struct {
	JobID       string `json:"job_id"`
	RunID       string `json:"run_id"`
	Reason      string `json:"reason"`
	JobLabel    string `json:"job_label,omitempty"`
	StartedAtMS int64  `json:"started_at_ms,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms,omitempty"`
}

// cronAttentionListResp is the GET /api/cron/attention response. Named struct
// (not map[string]any) keeps the 1Hz-poll endpoint on the cached reflect path,
// matching HandleRunsList's rationale.
type cronAttentionListResp struct {
	Items []cronAttentionItemView `json:"items"`
}

// HandleAttentionList serves GET /api/cron/attention — the §7.4 confirmation
// queue (failed-transport / orphaned runs of side-effecting jobs awaiting a
// human decision). Shares the runs rate limiter (FS scan, same bypass concern
// as the run history endpoints). Returns an empty items array (not 404) when
// the queue is empty so the drawer renders a deterministic empty state.
func (h *Handlers) HandleAttentionList(w http.ResponseWriter, r *http.Request) {
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		httputil.WriteJSON(w, cronAttentionListResp{Items: []cronAttentionItemView{}})
		return
	}
	rows := h.scheduler.ListSandboxAttention()
	out := make([]cronAttentionItemView, 0, len(rows))
	for _, it := range rows {
		out = append(out, cronAttentionItemView{
			JobID:       it.JobID,
			RunID:       it.RunID,
			Reason:      it.Reason,
			JobLabel:    osutil.SanitizeForLog(it.JobLabel, 256),
			StartedAtMS: it.StartedAtMS,
			CreatedAtMS: it.CreatedAtMS,
		})
	}
	httputil.WriteJSON(w, cronAttentionListResp{Items: out})
}

// HandleRunConfirm serves POST /api/cron/runs/{run_id}/confirm — the §7.4
// `确认已完成` action. Marks the run resolved without replaying (the operator
// has verified the side effect already landed). Write-rate-limited: it mutates
// queue state. Idempotent — confirming a run not in the queue returns 200.
func (h *Handlers) HandleRunConfirm(w http.ResponseWriter, r *http.Request) {
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}
	runID, ok := h.attentionRunID(w, r)
	if !ok {
		return
	}
	if err := h.scheduler.ConfirmSandboxRun(runID); err != nil {
		slog.Debug("cron run confirm failed", "err", err)
		writeCronErr(w, http.StatusBadRequest, "confirm failed")
		return
	}
	slog.Info("cron sandbox run confirmed done via dashboard", "run_id", osutil.SanitizeForLog(runID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// cronReplayResp returns the new run's id so the dashboard can deep-link to it.
type cronReplayResp struct {
	Status   string `json:"status"`
	NewRunID string `json:"new_run_id"`
}

// HandleRunReplay serves POST /api/cron/runs/{run_id}/replay — the §7.3 「重放」
// + §7.4 `确认未完成，重放` action. Re-injects the run's input snapshot into a
// fresh microVM. The §6.2 rule-1 Stop-before-replay is embedded in
// ReplaySandboxRun, so a transport-failed run whose microVM cannot be confirmed
// dead returns 409 (ErrStopUnconfirmed) and does NOT replay. Requires job_id in
// the body (the snapshot is keyed by job+run).
func (h *Handlers) HandleRunReplay(w http.ResponseWriter, r *http.Request) {
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}
	runID, ok := h.attentionRunID(w, r)
	if !ok {
		return
	}
	var req struct {
		JobID string `json:"job_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeCronErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.JobID == "" {
		writeCronErr(w, http.StatusBadRequest, "job_id is required")
		return
	}
	if len(req.JobID) > maxCronIDLenDashboard || !cronpkg.IsValidID(req.JobID) {
		writeCronErr(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	newRunID, err := h.scheduler.ReplaySandboxRun(req.JobID, runID)
	if err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobNotSandbox):
			writeCronErr(w, http.StatusConflict, "job is not at sandbox placement")
		case errors.Is(err, cronpkg.ErrNoSnapshot):
			writeCronErr(w, http.StatusUnprocessableEntity, "run has no input snapshot to replay")
		case errors.Is(err, cronpkg.ErrStopUnconfirmed):
			// §6.2 rule 1 unsatisfied: the original microVM's fate is unknown.
			// 409 Conflict — the operator can retry (Stop is idempotent).
			writeCronErr(w, http.StatusConflict, "original microVM termination unconfirmed; retry to replay safely")
		case errors.Is(err, cronpkg.ErrReplayInFlight):
			writeCronErr(w, http.StatusConflict, "job already has a run in flight")
		case errors.Is(err, cronpkg.ErrSandboxUnavailable):
			writeCronErr(w, http.StatusNotImplemented, "sandbox placement not configured")
		case errors.Is(err, cronpkg.ErrSchedulerStopped):
			writeCronErr(w, http.StatusServiceUnavailable, "scheduler stopped")
		default:
			slog.Debug("cron run replay failed", "err", err)
			writeCronErr(w, http.StatusBadRequest, "replay failed")
		}
		return
	}
	slog.Info("cron sandbox run replayed via dashboard",
		"orig_run_id", osutil.SanitizeForLog(runID, cronpkg.MaxIDLen),
		"new_run_id", osutil.SanitizeForLog(newRunID, cronpkg.MaxIDLen))
	httputil.WriteJSON(w, cronReplayResp{Status: "replaying", NewRunID: newRunID})
}

// attentionRunID extracts + shape-validates the {run_id} path param shared by
// confirm/replay. Writes the 4xx + returns ok=false on any validation failure.
func (h *Handlers) attentionRunID(w http.ResponseWriter, r *http.Request) (string, bool) {
	runID := r.PathValue("run_id")
	if runID == "" {
		writeCronErr(w, http.StatusBadRequest, "run_id is required")
		return "", false
	}
	if len(runID) > runIDLenLimit {
		writeCronErr(w, http.StatusBadRequest, "run_id too long")
		return "", false
	}
	if !cronpkg.IsValidID(runID) {
		writeCronErr(w, http.StatusBadRequest, "run_id must be lowercase hex")
		return "", false
	}
	return runID, true
}
