package cron

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// runIDLenLimit caps run_id length; run IDs and job IDs share one generator and
// store, so they share cronpkg.MaxIDLen.
const runIDLenLimit = cronpkg.MaxIDLen

// GET /api/cron/runs?job_id=&limit=&before= returns CronRun summaries for one
// job, newest first. limit defaults to 50 (clamped to DefaultRunsKeepCount);
// before is a unix-ms paging cursor. next_before is omitted on the last page.
func (h *Handlers) HandleRunsList(w http.ResponseWriter, r *http.Request) {
	if h.deps.Scheduler == nil {
		// Explicit empty slice so json.Marshal emits `[]`, not null.
		httputil.WriteJSON(w, cronRunsListResp{Runs: []cronRunSummaryView{}})
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}
	if len(jobID) > maxCronIDLenDashboard {
		http.Error(w, "job_id too long", http.StatusBadRequest)
		return
	}
	if !cronpkg.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if len(raw) > 4 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if n > cronpkg.DefaultRunsKeepCount {
			n = cronpkg.DefaultRunsKeepCount
		}
		limit = n
	}
	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		if len(raw) > 16 {
			http.Error(w, "before must be a unix-ms integer", http.StatusBadRequest)
			return
		}
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ms <= 0 {
			http.Error(w, "before must be a unix-ms integer", http.StatusBadRequest)
			return
		}
		before = time.UnixMilli(ms)
	}

	rows := h.deps.Scheduler.ListRuns(jobID, limit, before)
	out := make([]cronRunSummaryView, 0, len(rows))
	for _, r := range rows {
		out = append(out, cronSummaryToView(r))
	}
	resp := cronRunsListResp{Runs: out}
	// next_before only when this page was full; a partial page may still mean
	// "no more" because older runs may have been GC'd.
	if len(out) == limit && len(out) > 0 {
		resp.NextBefore = out[len(out)-1].StartedAt
	}
	httputil.WriteJSON(w, resp)
}

// GET /api/cron/runs/{run_id}?job_id=... returns the full CronRun (Prompt +
// Result + ErrorMsg): 404 when missing, 500 when the record is corrupt.
func (h *Handlers) HandleRunDetail(w http.ResponseWriter, r *http.Request) {
	if h.deps.Scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}
	runID := r.PathValue("run_id")
	if runID == "" {
		http.Error(w, "run_id is required", http.StatusBadRequest)
		return
	}
	if len(runID) > runIDLenLimit {
		http.Error(w, "run_id too long", http.StatusBadRequest)
		return
	}
	if !cronpkg.IsValidID(runID) {
		http.Error(w, "run_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}
	if len(jobID) > maxCronIDLenDashboard {
		http.Error(w, "job_id too long", http.StatusBadRequest)
		return
	}
	if !cronpkg.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	run, err := h.deps.Scheduler.Run(jobID, runID)
	if err != nil {
		if errors.Is(err, cronpkg.ErrCorruptRun) {
			slog.Warn("cron run record corrupt", "job_id", jobID, "run_id", runID, "err", err)
			http.Error(w, "run record corrupt", http.StatusInternalServerError)
			return
		}
		// Any non-corrupt error (incl. fs.ErrNotExist) is "not found" so the
		// response cannot leak filesystem layout to a remote caller.
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	// Cross-ownership check: runstore.Store.Get keys on the disk path, but a future
	// refactor that loosens the key must not expose another job's run here.
	if run.JobID != jobID {
		slog.Warn("cron run detail: job_id mismatch", "url_job_id", jobID, "run_job_id", run.JobID, "run_id", runID)
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	// SanitizeForLog Prompt / WorkDir / SessionID read off disk: a record
	// persisted before the validators were tightened (or hand-edited) can carry
	// control / bidi runes. Result and ErrorMsg are sanitised before persistence.
	out := cronRunDetailView{
		RunID:       run.RunID,
		JobID:       run.JobID,
		State:       string(run.State),
		Trigger:     string(run.Trigger),
		StartedAt:   run.StartedAt.UnixMilli(),
		DurationMS:  run.DurationMS,
		SessionID:   osutil.SanitizeForLog(run.SessionID, 64),
		Prompt:      osutil.SanitizeForLog(run.Prompt, cronpkg.MaxPromptBytes),
		WorkDir:     osutil.SanitizeForLog(run.WorkDir, cronpkg.MaxWorkDirLen),
		Fresh:       run.Fresh,
		Result:      run.Result,
		ResultBytes: run.ResultBytes,
		ErrorClass:  string(run.ErrorClass),
		ErrorMsg:    run.ErrorMsg,
	}
	if !run.EndedAt.IsZero() {
		out.EndedAt = run.EndedAt.UnixMilli()
	}
	httputil.WriteJSON(w, out)
}
