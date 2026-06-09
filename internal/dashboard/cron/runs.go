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

// runIDLenLimit caps the run_id query parameter length. Run IDs and job
// IDs share the same generator class (hex with headroom for future
// entropy bumps) and the same on-disk JSON store, so they share the
// cronpkg.MaxIDLen constant. R230B-CR-1: previously a separate const was
// kept "in case of divergence", but no concrete plan exists for run IDs
// to grow longer than job IDs, and two parallel constants drifted in
// review with no source of truth. Reuse one constant; revisit if a real
// divergence requirement appears.
const runIDLenLimit = cronpkg.MaxIDLen

// GET /api/cron/runs?job_id=&limit=&before=
//
// Returns CronRun summaries for one job, newest first. limit default 50,
// clamped to [1, cronpkg.DefaultRunsKeepCount]. before is unix-ms; only runs
// strictly older than that timestamp are returned (paging cursor).
//
// Response shape:
//
//	{ "runs":[ { run_id, state, trigger, started_at, ended_at,
//	             duration_ms, session_id, error_class } ],
//	  "next_before": <unix-ms>   // omitted when no more pages
//	}
//
// Authenticated; no per-job ACL beyond the global dashboard auth gate
// (mirrors HandleList's policy).
func (h *Handlers) HandleRunsList(w http.ResponseWriter, r *http.Request) {
	// R222-SEC-3: gate per-IP before any scheduler / FS work so an attacker
	// holding a stolen dashboard token cannot enumerate the run history at
	// unbounded rate. Nil-guarded for hand-built tests.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		// R230B-CR-3: same byte shape as the map[string]any fast path —
		// `{"runs":[]}`. Explicit empty slice keeps json.Marshal off the
		// nil → "null" rendering branch.
		httputil.WriteJSON(w, cronRunsListResp{Runs: []cronRunSummaryView{}})
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		writeCronErr(w, http.StatusBadRequest, "job_id is required")
		return
	}
	if len(jobID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "job_id too long")
		return
	}
	if !cronpkg.IsValidID(jobID) {
		writeCronErr(w, http.StatusBadRequest, "job_id must be lowercase hex")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if len(raw) > 4 {
			writeCronErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeCronErr(w, http.StatusBadRequest, "limit must be a positive integer")
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
			writeCronErr(w, http.StatusBadRequest, "before must be a unix-ms integer")
			return
		}
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ms <= 0 {
			writeCronErr(w, http.StatusBadRequest, "before must be a unix-ms integer")
			return
		}
		before = time.UnixMilli(ms)
	}

	rows := h.scheduler.ListRuns(jobID, limit, before)
	out := make([]cronRunSummaryView, 0, len(rows))
	for _, r := range rows {
		out = append(out, cronSummaryToView(r))
	}
	// R230B-CR-3: named struct in place of map[string]any keeps this 1Hz-poll
	// endpoint on the cached reflect path (one-time alloc) instead of paying
	// the per-call map iteration + interface boxing each request.
	resp := cronRunsListResp{Runs: out}
	// next_before: emit only when this page was full (caller may have more).
	// Conservative: a partial page can still indicate "no more" because runs
	// older than this batch may have been GC'd; we let the dashboard treat
	// next_before as "fetch older than this" hint.
	if len(out) == limit && len(out) > 0 {
		resp.NextBefore = out[len(out)-1].StartedAt
	}
	httputil.WriteJSON(w, resp)
}

// GET /api/cron/runs/{run_id}?job_id=...
//
// Returns the full CronRun (Prompt + Result + ErrorMsg). 404 when missing,
// 500 with "corrupt record" message when the file exists but fails to
// parse / exceeds size cap. Used by the dashboard detail drawer.
func (h *Handlers) HandleRunDetail(w http.ResponseWriter, r *http.Request) {
	// R222-SEC-3: same per-IP gate as HandleRunsList. Detail reads also do
	// FS I/O (read JSON file from disk) so they share the limiter and
	// budget, not separate buckets — a single bucket prevents bypass-via-
	// alternate-endpoint when both URLs share an identical token.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}
	// Path param: /api/cron/runs/{run_id}; PathValue is supplied by the
	// http.ServeMux pattern at registration time. Defensive in case the
	// pattern is changed without updating this handler.
	runID := r.PathValue("run_id")
	if runID == "" {
		writeCronErr(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if len(runID) > runIDLenLimit {
		writeCronErr(w, http.StatusBadRequest, "run_id too long")
		return
	}
	if !cronpkg.IsValidID(runID) {
		writeCronErr(w, http.StatusBadRequest, "run_id must be lowercase hex")
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		writeCronErr(w, http.StatusBadRequest, "job_id is required")
		return
	}
	if len(jobID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "job_id too long")
		return
	}
	if !cronpkg.IsValidID(jobID) {
		writeCronErr(w, http.StatusBadRequest, "job_id must be lowercase hex")
		return
	}
	run, err := h.scheduler.Run(jobID, runID)
	if err != nil {
		if errors.Is(err, cronpkg.ErrCorruptRun) {
			slog.Warn("cron run record corrupt", "job_id", jobID, "run_id", runID, "err", err)
			writeCronErr(w, http.StatusInternalServerError, "run record corrupt")
			return
		}
		// Default: treat any non-corrupt error (incl. fs.ErrNotExist)
		// as "not found" — distinguishing "not exist" vs "perm denied"
		// would leak filesystem layout to a remote caller.
		writeCronErr(w, http.StatusNotFound, "run not found")
		return
	}
	// SanitizeForLog the Prompt + WorkDir fields read off disk: dashboard
	// validate* gates already strip control / bidi characters at the write
	// edge, but a CronRun persisted before the policy was tightened (or
	// hand-edited on disk) can carry runes that would render dangerously
	// in the dashboard. Result/ErrorMsg are already sanitised inside
	// recordResultP0WithSanitised before persistence.
	// [R112714-SEC-5] SanitizeForLog SessionID: a CronRun record persisted
	// before the validator was tightened (or hand-edited on disk) can carry
	// control/bidi characters. UUID session IDs are at most 36 chars;
	// clamp to 64 to give headroom for future formats without amplifying
	// an injected payload.
	out := cronRunDetailView{
		RunID:       run.RunID,
		JobID:       run.JobID,
		State:       string(run.State),
		Trigger:     string(run.Trigger),
		StartedAt:   run.StartedAt.UnixMilli(),
		DurationMS:  run.DurationMS,
		SessionID:   osutil.SanitizeForLog(run.SessionID, 64),
		Prompt:      osutil.SanitizeForLog(run.Prompt, cronpkg.MaxPromptBytes),
		WorkDir:     osutil.SanitizeForLog(run.WorkDir, maxCronWorkDirBytesDashboard),
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
