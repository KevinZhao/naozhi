package cron

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// runIDLenLimit caps run_id length; run IDs and job IDs share one generator and
// store, so they share cronpkg.MaxIDLen.
const runIDLenLimit = cronpkg.MaxIDLen

// GET /api/cron/runs?job_id=&limit=&before= returns CronRun summaries for one
// job, newest first. limit defaults to 50 (clamped to DefaultRunsKeepCount);
// before is a unix-ms paging cursor. next_before is omitted on the last page.
func (h *Handlers) HandleRunsList(w http.ResponseWriter, r *http.Request) {
	// Gate per-IP before any scheduler / FS work (stolen-token enumeration).
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
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

	rows := h.scheduler.ListRuns(jobID, limit, before)
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
	// Same per-IP bucket as HandleRunsList so the alternate URL cannot bypass it.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
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
	run, err := h.scheduler.Run(jobID, runID)
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
	// Cross-ownership check: runStore.Get keys on the disk path, but a future
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
		WorkDir:     osutil.SanitizeForLog(run.WorkDir, maxCronWorkDirBytesDashboard),
		Fresh:       run.Fresh,
		Result:      run.Result,
		ResultBytes: run.ResultBytes,
		ErrorClass:  string(run.ErrorClass),
		ErrorMsg:    run.ErrorMsg,
		ReplayOf:    run.ReplayOf,
	}
	if !run.EndedAt.IsZero() {
		out.EndedAt = run.EndedAt.UnixMilli()
	}
	if m := run.SandboxMeta; m != nil {
		// RuntimeARN is non-secret, but sanitise in case of a hand-edited record.
		out.Sandbox = &cronRunSandboxView{
			RuntimeARN:      osutil.SanitizeForLog(m.RuntimeARN, 256),
			ImageVersion:    osutil.SanitizeForLog(m.ImageVersion, 128),
			ExitStatus:      m.ExitStatus,
			CostUSD:         m.CostUSD,
			DurationMS:      m.DurationMS,
			MemoryPeakBytes: m.MemoryPeakBytes,
		}
	}
	httputil.WriteJSON(w, out)
}

// sandboxEventsMaxResponse caps envelope frames returned by GET .../events; the
// dashboard renders the run's opening with a truncated flag for the tail.
const sandboxEventsMaxResponse = 2000

// cronRunEventsResp is the wire shape for GET /api/cron/runs/{run}/events:
// raw stream envelopes re-emitted verbatim so the dashboard reuses the
// local-session event renderer (RFC §7.3).
type cronRunEventsResp struct {
	Events    []json.RawMessage `json:"events"`
	Truncated bool              `json:"truncated,omitempty"`
}

// HandleRunEvents serves GET /api/cron/runs/{run_id}/events?job_id= — the
// persisted sandbox event log (RFC §6.1/§7.3). Returns an empty events array
// (not 404) for a run with no log so the dashboard renders consistently.
func (h *Handlers) HandleRunEvents(w http.ResponseWriter, r *http.Request) {
	// The events path is I/O-heavy like the transcript endpoint, so it uses
	// transcriptLimiter (sharing runsLimiter lets either starve the other);
	// falls back to runsLimiter for fixtures without a transcriptLimiter.
	limiter := h.transcriptLimiter
	if limiter == nil {
		limiter = h.runsLimiter
	}
	if limiter != nil && !limiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron run events rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		httputil.WriteJSONStatus(w, http.StatusNotImplemented, map[string]string{"error": "cron not configured"})
		return
	}
	runID := r.PathValue("run_id")
	if runID == "" || len(runID) > runIDLenLimit || !cronpkg.IsValidID(runID) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid run_id"})
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" || len(jobID) > maxCronIDLenDashboard || !cronpkg.IsValidID(jobID) {
		httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid job_id"})
		return
	}

	lines, truncated, err := h.scheduler.SandboxRunEvents(jobID, runID, sandboxEventsMaxResponse)
	if errors.Is(err, cronpkg.ErrSandboxEventsBusy) {
		// Semaphore saturated: fail fast (503) rather than serve a partial stream.
		httputil.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "cron run events busy"})
		return
	}
	if err != nil {
		// A scan error still returns the partial head; serve what we have.
		slog.Warn("cron sandbox: run events read error", "job_id", jobID, "run_id", runID, "err", err)
	}
	// Redaction contract: every NDJSON line is scrubbed of secrets (tool-call
	// input can echo API keys / Bearer tokens) and then of absolute host paths
	// (filesystem layout leak), same order as sanitizeWireText. Both redactors
	// can break JSON at the edges, so redactRawJSON re-applies per string value.
	events := make([]json.RawMessage, len(lines))
	for i, ln := range lines {
		events[i] = redactRawJSON(string(ln), redactEventText)
	}
	httputil.WriteJSON(w, cronRunEventsResp{Events: events, Truncated: truncated})
}

// redactEventText is the per-string redaction chain for sandbox run events.
func redactEventText(s string) string {
	return osutil.RedactAbsolutePaths(textutil.RedactSecrets(s))
}

// cronRunSnapshotResp is the wire shape for GET /api/cron/runs/{run}/snapshot
// (§7.3 input-snapshot panel). Secrets appear ONLY as reference names
// (secret_refs), never values (§5.1 red line).
type cronRunSnapshotResp struct {
	Available    bool     `json:"available"`
	Prompt       string   `json:"prompt,omitempty"`
	PromptHash   string   `json:"prompt_hash,omitempty"`
	Model        string   `json:"model,omitempty"`
	ImageVersion string   `json:"image_version,omitempty"`
	SecretRefs   []string `json:"secret_refs,omitempty"`
}

// HandleRunSnapshot serves GET /api/cron/runs/{run_id}/snapshot?job_id= (§7.3).
// Returns {available:false} (not 404) for a run with no snapshot so the panel
// renders a deterministic "unavailable" state.
func (h *Handlers) HandleRunSnapshot(w http.ResponseWriter, r *http.Request) {
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}
	runID := r.PathValue("run_id")
	if runID == "" || len(runID) > runIDLenLimit || !cronpkg.IsValidID(runID) {
		http.Error(w, "invalid run_id", http.StatusBadRequest)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" || len(jobID) > maxCronIDLenDashboard || !cronpkg.IsValidID(jobID) {
		http.Error(w, "invalid job_id", http.StatusBadRequest)
		return
	}

	man, ok, err := h.scheduler.SandboxRunSnapshotManifest(jobID, runID)
	if err != nil {
		slog.Warn("cron sandbox: snapshot manifest read error", "job_id", jobID, "run_id", runID, "err", err)
		httputil.WriteJSON(w, cronRunSnapshotResp{Available: false})
		return
	}
	if !ok {
		httputil.WriteJSON(w, cronRunSnapshotResp{Available: false})
		return
	}
	// Cross-ownership check, mirroring HandleRunDetail: path isolation already
	// prevents cross-job reads, but a future path-convention change must not
	// silently expose another job's snapshot.
	if man.JobID != "" && man.JobID != jobID {
		slog.Warn("cron snapshot: job_id mismatch", "url_job_id", jobID, "man_job_id", man.JobID, "run_id", runID)
		httputil.WriteJSON(w, cronRunSnapshotResp{Available: false})
		return
	}
	prompt, perr := h.scheduler.SandboxRunSnapshotPrompt(man.PromptHash)
	if perr != nil {
		slog.Warn("cron sandbox: snapshot prompt read error", "job_id", jobID, "run_id", runID, "err", perr)
		// Still return the manifest metadata; prompt blob may have been GC'd.
	}
	// SanitizeForLog each secret ref name: a hand-edited manifest can carry
	// control/bidi runes. Ref names are short identifiers; clamp to 256.
	var secretRefs []string
	if len(man.SecretRefs) > 0 {
		secretRefs = make([]string, len(man.SecretRefs))
		for i, ref := range man.SecretRefs {
			secretRefs[i] = osutil.SanitizeForLog(ref, 256)
		}
	}
	httputil.WriteJSON(w, cronRunSnapshotResp{
		Available: true,
		// Sanitise defensively in case the blob predates the write-edge policy.
		Prompt: osutil.SanitizeForLog(prompt, cronpkg.MaxPromptBytes),
		// PromptHash should be 64 hex chars but the manifest is hand-editable.
		PromptHash:   osutil.SanitizeForLog(man.PromptHash, 64),
		Model:        osutil.SanitizeForLog(man.Model, 128),
		ImageVersion: osutil.SanitizeForLog(man.ImageVersion, 128),
		SecretRefs:   secretRefs,
	})
}
