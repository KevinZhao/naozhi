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
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}
	// Path param: /api/cron/runs/{run_id}; PathValue is supplied by the
	// http.ServeMux pattern at registration time. Defensive in case the
	// pattern is changed without updating this handler.
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
		// Default: treat any non-corrupt error (incl. fs.ErrNotExist)
		// as "not found" — distinguishing "not exist" vs "perm denied"
		// would leak filesystem layout to a remote caller.
		http.Error(w, "run not found", http.StatusNotFound)
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
		ReplayOf:    run.ReplayOf,
	}
	if !run.EndedAt.IsZero() {
		out.EndedAt = run.EndedAt.UnixMilli()
	}
	if m := run.SandboxMeta; m != nil {
		// RFC §7.3 meta bar. RuntimeARN is operator-facing config (no
		// secret), but SanitizeForLog it defensively in case a hand-edited
		// record carries control bytes that would render dangerously.
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

// sandboxEventsMaxResponse caps how many envelope frames GET .../events
// returns. Matches the cron-side default; the dashboard renders the run's
// opening (boot + early turns — most useful for "what happened / where did
// it break"), with a truncated flag for the tail.
const sandboxEventsMaxResponse = 2000

// cronRunEventsResp is the wire shape for GET /api/cron/runs/{run}/events.
// Events are raw stream envelopes (kind=cli/boot/exit/meta…) re-emitted
// verbatim as JSON so the dashboard renders them with the same component
// the local-session event stream uses (RFC §7.3 "identical message render").
type cronRunEventsResp struct {
	Events    []json.RawMessage `json:"events"`
	Truncated bool              `json:"truncated,omitempty"`
}

// HandleRunEvents serves GET /api/cron/runs/{run_id}/events?job_id= — the
// persisted sandbox event log (RFC §6.1/§7.3). Shares the runs rate limiter
// (FS I/O, same bypass concern as detail). Returns an empty events array for
// a run with no log (local run / events-disabled deploy) rather than 404, so
// the dashboard renders an empty stream consistently.
func (h *Handlers) HandleRunEvents(w http.ResponseWriter, r *http.Request) {
	// R20260613-SEC-1: use the dedicated transcriptLimiter rather than the
	// shared runsLimiter. The events path uses bufio.Scanner (16.5 MB max
	// token) and is I/O-heavy like the transcript endpoint — sharing one
	// bucket lets either endpoint starve the other. Fall back to runsLimiter
	// for legacy hand-rolled Handlers fixtures without a transcriptLimiter.
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
	if err != nil {
		// A scan error still returns the partial head (lines may be non-nil);
		// log + serve what we have rather than hiding a healthy opening.
		slog.Warn("cron sandbox: run events read error", "job_id", jobID, "run_id", runID, "err", err)
	}
	// R20260613-SEC-1: redact secrets from each NDJSON line before serving to
	// the dashboard. The sandbox event log can contain tool-call input that
	// echoes environment variables (e.g. ANTHROPIC_API_KEY, Bearer tokens).
	// textutil.RedactSecrets replaces known secret-token shapes with [REDACTED]
	// while preserving JSON validity: secret chars are alphanumeric/-/_ which
	// are all legal inside a JSON string, and [REDACTED] is equally legal there.
	events := make([]json.RawMessage, len(lines))
	for i, ln := range lines {
		redacted := textutil.RedactSecrets(string(ln))
		events[i] = json.RawMessage(redacted)
	}
	httputil.WriteJSON(w, cronRunEventsResp{Events: events, Truncated: truncated})
}

// cronRunSnapshotResp is the wire shape for GET /api/cron/runs/{run}/snapshot
// — the §7.3 input-snapshot panel (debug / replay preview). Carries the
// content-addressed manifest fields plus the resolved prompt text. Secrets
// appear ONLY as reference names (secret_refs), never values (§5.1 red line);
// the panel renders the names and the UI never has a value to leak.
type cronRunSnapshotResp struct {
	Available    bool     `json:"available"`
	Prompt       string   `json:"prompt,omitempty"`
	PromptHash   string   `json:"prompt_hash,omitempty"`
	Model        string   `json:"model,omitempty"`
	ImageVersion string   `json:"image_version,omitempty"`
	SecretRefs   []string `json:"secret_refs,omitempty"`
}

// HandleRunSnapshot serves GET /api/cron/runs/{run_id}/snapshot?job_id= —
// the §7.3 input-snapshot view. Shares the runs rate limiter (FS I/O).
// Returns {available:false} (not 404) for a run with no snapshot (local run
// / snapshots-disabled / pre-snapshot run) so the panel renders a
// deterministic "unavailable" state.
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
	prompt, perr := h.scheduler.SandboxRunSnapshotPrompt(man.PromptHash)
	if perr != nil {
		slog.Warn("cron sandbox: snapshot prompt read error", "job_id", jobID, "run_id", runID, "err", perr)
		// Still return the manifest metadata; prompt blob may have been GC'd.
	}
	httputil.WriteJSON(w, cronRunSnapshotResp{
		Available: true,
		// Prompt was validated non-control at the cron write edge, but
		// SanitizeForLog defensively in case a blob predates that policy.
		Prompt:       osutil.SanitizeForLog(prompt, cronpkg.MaxPromptBytes),
		PromptHash:   man.PromptHash,
		Model:        osutil.SanitizeForLog(man.Model, 128),
		ImageVersion: osutil.SanitizeForLog(man.ImageVersion, 128),
		SecretRefs:   man.SecretRefs,
	})
}
