package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Validation helpers (validateStringField / validateCron* / stringFieldPolicy)
// and the maxCron*/validNotifyPlatforms constants live in
// dashboard_cron_validate.go. Split out of this file in #1281 so the cron
// HTTP handler bodies are not interleaved with their input-scrub primitives.

// JSON wire types (cronRunSummaryView / cronJobView / cronListResp /
// cronCreateResp / cronCurrentRunView / cronRunCountersView /
// cronRunDetailView / cronNotifyDefaultView / cronRunsListResp /
// cronPreviewResp / cronUpdateResp), the cronSummaryToView projection,
// and formatTZOffset moved to dashboard_cron_view.go (#1281). Validators
// (validateStringField / validateCron* / stringFieldPolicy) and the
// associated maxCron* / validNotifyPlatforms constants live in
// dashboard_cron_validate.go.

// CronHandlers groups the cron job management API endpoints.
type CronHandlers struct {
	scheduler   *cron.Scheduler
	allowedRoot string
	// claudeDir is the absolute path to ~/.claude. Used by handleRunTranscript
	// to locate the JSONL conversation file for a given run's session_id.
	// Empty disables the transcript endpoint (returns fallback:"missing").
	// cron-dashboard-redesign P2a §4.4.3.
	claudeDir string
	// runsLimiter caps how often a single authenticated caller can hit
	// `/api/cron/runs`, `/api/cron/runs/{run_id}`, and the transcript
	// endpoint. All three fan out filesystem I/O against the per-job runs
	// directory or the JSONL on disk; an attacker holding a stolen dashboard
	// token can otherwise enumerate the entire run history at unbounded
	// rate. R222-SEC-3.
	//
	// Nil-guarded so tests built via newCronHandlersForTest (and other
	// hand-rolled CronHandlers instances) skip the gate; wiring lives in
	// server.New.
	runsLimiter *ipLimiter

	// listLimiter caps GET /api/cron — the dashboard's primary 1 Hz
	// polling endpoint. R242-CR-3: the runs/transcript endpoints are
	// already rate-limited because each call fans out FS I/O, but the
	// list endpoint runs ListAllJobsWithNextRun + RecentRuns(5) per job
	// and per-call cost grows with N (jobs) × 5 (recent run files). A
	// stolen token can otherwise burn IO at unbounded rate while the
	// runs limiter sits idle.
	//
	// Sustained 2 req/s with burst 30 — generous enough that a single
	// dashboard tab refresh storm (open + immediate filter change at
	// the top of a minute) doesn't trip, but caps a parallel-poll
	// attacker at ~2 req/s steady-state per source IP. Mirrors the
	// shape of runsLimiter (rate.Every + burst) so ops familiarity
	// transfers; the higher steady rate reflects that this endpoint is
	// the dashboard's heartbeat.
	//
	// Nil-guarded just like runsLimiter so newCronHandlersForTest paths
	// skip the gate. Wiring lives in server.New.
	listLimiter *ipLimiter

	// writeLimiter caps per-IP rate of authenticated cron-write/control
	// endpoints that fan out side-effects beyond a cheap read:
	//
	//   - POST /api/cron/trigger  spawns the job's claude CLI subprocess
	//     and may send IM notifications, so loop-triggering is a realistic
	//     DoS / amplification vector even for a logged-in caller.
	//   - GET  /api/cron/preview   parses cron expressions in a tight loop
	//     up to N=10 next-run computations; cheaper than trigger but still
	//     not a heartbeat endpoint and shouldn't be unbounded.
	//
	// Sustained 30 req/min with burst 6 is generous for legitimate UI usage
	// (a single user form-edit cycle hits preview a handful of times per
	// minute) while capping a stolen-token attacker at one trigger every
	// 2 s steady-state. Single shared bucket per IP keeps the wiring
	// simple and the per-IP control surface uniform.
	//
	// Nil-guarded so newCronHandlersForTest paths skip the gate; wiring
	// lives in server.New. [R247-SEC-2 / R247-SEC-3]
	writeLimiter *ipLimiter

	// missedCache memoises HasMissedSchedule verdicts so the dashboard's
	// 1 Hz handleList path doesn't re-Parse the cron expression for every
	// job on every poll. Without the cache, robfig/cron's regexp NFA
	// build runs N (jobs) × T (parallel tabs) times per second; with it,
	// steady-state cost falls to N parses per missedCacheTTL because
	// cache hits skip Parse entirely. The verdict depends on (Schedule,
	// LastRunAt, startedAt) plus `now` modulo TTL — schedule edits and
	// scheduler restarts invalidate via the composite key (see the
	// missedScheduleVerdict helper); LastRunAt advances invalidate via
	// the lastRunNanos guard so a job that just ran does not keep an
	// outdated "missed" verdict for a full second. R245-PERF-4 (#857).
	missedCacheMu sync.Mutex
	missedCache   map[string]missedVerdict

	// transcriptSem caps concurrent /api/cron/runs/{run_id}/transcript
	// requests across the whole process. R243-SEC-12 (#798): each
	// in-flight transcript holds a 256 KB bufio.Scanner buffer plus
	// the LimitReader's 8 MB read budget, so the per-IP runsLimiter
	// alone is not enough — N distinct authenticated operators can
	// each saturate their own bucket and collectively park N×8 MB
	// of file-mapped pages plus N×256 KB of scanner buffers in
	// memory. The semaphore puts a process-wide ceiling on that
	// concurrency so memory cannot grow unbounded with operator
	// count. Excess requests receive 503 immediately, mirroring the
	// transcribeSemCap pattern in dashboard_transcribe.go. Nil leaves
	// the gate disabled (newCronHandlersForTest paths) so legacy
	// hand-rolled fixtures keep compiling.
	transcriptSem chan struct{}
}

// missedVerdict, missedCacheTTL, missedCacheCap, and the
// (h *CronHandlers).missedScheduleVerdict helper moved to
// dashboard_cron_missed.go (#1281). The CronHandlers struct still owns the
// missedCacheMu / missedCache fields — only the cache value type, the
// tuning constants, and the lookup-and-store helper relocated.

// recentRunsPerJob is the per-job RecentRuns cap embedded in handleList's
// list response. Mirrors the literal previously inlined as
// scheduler.RecentRuns(j.ID, 5) so the bounded fan-out helper carries the
// same wire-shape contract the dashboard JS reads. Tooltip-bound; the
// dashboard's per-job runs detail drawer uses GET /api/cron/runs for
// richer pagination. R236-PERF-08 (#525).
const recentRunsPerJob = 5

// batchRecentRunsWorkers caps the concurrent RecentRuns goroutines spun up
// by batchRecentRuns. Sized 8 so a single 1 Hz dashboard poll on a 50-job
// install fans out across 8-way parallelism (≈ 6 jobs/worker, sub-ms wall
// time once the runStore cache is warm) without flooding sync.Map.Load
// contention or the Go scheduler with hundreds of short-lived goroutines.
// Above ~16 readers the per-jobLock TryLock + recentCacheEntry.mu acquire
// chain contends on Go runtime spinlocks and the marginal speedup goes
// negative; 8 stays comfortably below that knee. R236-PERF-08 (#525).
const batchRecentRunsWorkers = 8

// batchRecentRuns fans out scheduler.RecentRuns lookups across at most
// batchRecentRunsWorkers goroutines and returns one result per input job
// in input-index order. Used by handleList to drop the previous N×serial
// per-job lock acquire (the per-recentCacheEntry.mu chain) — under load,
// 1 Hz polls on a 50-job install no longer stall behind the slowest
// jobLock pass because at most W jobs queue at the runStore lock at any
// instant.
//
// Result slice is always len(jobs); entries may be nil for jobs with no
// run history. Caller is responsible for the nil-len check before
// projecting cron summaries onto the wire shape.
//
// Single-call cost vs. inline serial loop:
//   - Cold scheduler: serial = N × (warmCache + ringSnapshot); fan-out =
//     ⌈N/W⌉ × max(per-job warmCache); typical 4-8× speedup at W=8.
//   - Warm cache:    serial = N × ringSnapshot copy; fan-out = ⌈N/W⌉ ×
//     ringSnapshot copy; modest (~3×) speedup but tail-latency improves.
//
// Nil-safe: when h.scheduler is nil the caller short-circuits earlier; we
// also guard inside so a future caller can pass an empty jobs slice and
// receive nil without panic.
//
// R236-PERF-08 (#525).
func (h *CronHandlers) batchRecentRuns(jobs []cron.JobWithNextRun, n int) [][]cron.CronRunSummary {
	if len(jobs) == 0 || h.scheduler == nil {
		return nil
	}
	out := make([][]cron.CronRunSummary, len(jobs))
	// Tasks queue: each worker pulls the next index and fans out to the
	// scheduler. Channel-of-int keeps the work distribution self-balancing
	// — a slow per-job lookup (cold cache, slow disk) does not pin a
	// single worker on a single job while its peers idle.
	tasks := make(chan int, len(jobs))
	for i := range jobs {
		tasks <- i
	}
	close(tasks)
	workers := batchRecentRunsWorkers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for idx := range tasks {
				out[idx] = h.scheduler.RecentRuns(jobs[idx].Job.ID, n)
			}
		}()
	}
	wg.Wait()
	return out
}

// GET /api/cron — list all cron jobs (unscoped, admin view).
func (h *CronHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	// R242-CR-3: gate per-IP before the scheduler/FS work so a stolen
	// dashboard token cannot enumerate the job list (with embedded
	// RecentRuns(5) per job) at unbounded rate. Mirrors runsLimiter
	// usage in handleRunsList. Nil-guarded for hand-built tests.
	if h.listLimiter != nil && !h.listLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron list rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		// R230B-CR-3: keep wire shape `{"jobs":[]}` byte-equal to the prior
		// map[string]any{"jobs": []any{}} fast path. Explicit empty slice
		// (not nil) so json.Marshal emits `[]` rather than `null`.
		writeJSON(w, cronListResp{Jobs: []cronJobView{}})
		return
	}

	jobs := h.scheduler.ListAllJobsWithNextRun()
	// R241-PERF-1: capture once outside the loop; each non-paused job called
	// time.Now() and h.scheduler.StartedAt() (an atomic load) independently,
	// yielding O(n) syscalls/atomics for an effectively-constant value.
	now := time.Now()
	startedAt := h.scheduler.StartedAt()

	// R236-PERF-08 (#525): pre-fetch RecentRuns for every job in parallel
	// so the 1 Hz dashboard poll's serial fan-out across N jobs does not
	// stall behind the per-job recentCacheEntry.mu acquire chain. The
	// previous shape called scheduler.RecentRuns inside the per-job loop
	// below, so under load N tabs × N jobs × per-job lock acquire serialised
	// the entire list response on the slowest jobLock / warmCache pass.
	// The fan-out runs at most batchRecentRunsWorkers goroutines so the
	// scheduler's runStore is not flooded with goroutines on a 200-job
	// install (sync.Map.Load contention dominates above ~16 readers); the
	// per-job result lands in a pre-sized slice keyed by job index so the
	// main loop below sees a deterministic order with no map allocation.
	recentByIdx := h.batchRecentRuns(jobs, recentRunsPerJob)

	views := make([]cronJobView, 0, len(jobs))
	for idx, entry := range jobs {
		j := entry.Job
		// Prompt 不截断：dashboard.js 客户端 fuzzy-search 依赖完整 prompt
		// 内容（filterCronJobs 在 j.prompt 上做 substring match）。截断后
		// 搜索结果会假阴。8 KiB × 50 job = 400 KiB/响应 在 1 Hz 拉取下
		// 是已知开销，待后续移到 server-side search 后再优化。
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
			LastErrorClass: string(j.LastErrorClass),
			Notify:         j.Notify,
			FreshContext:   j.FreshContext,
			Backend:        j.Backend,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if !entry.NextRun.IsZero() {
			v.NextRun = entry.NextRun.UnixMilli()
		}
		// missed-schedule 检测：cron-v2-polish §3.3 Increment C。
		// 只对非 paused 的 job 判定——paused 的任务用户主动停了，错过
		// 是预期行为不应告警。R245-PERF-4 (#857): route through
		// missedScheduleVerdict so the cron expression Parse is memoised
		// across the 1 Hz dashboard poll cadence — N jobs × T tabs no
		// longer fans out to N×T regexp NFA builds per second.
		if !j.Paused {
			if missed, prevAt := h.missedScheduleVerdict(&j, now, startedAt); missed {
				v.Missed = true
				v.MissedSince = prevAt.UnixMilli()
			}
		}
		// CurrentRun & Stats — P0 cron-run-history。CurrentRun 只在 job 正
		// 在执行时返回；空 stats 也省略以减少线上 noise。
		if cur, ok := h.scheduler.CurrentRun(j.ID); ok {
			v.CurrentRun = &cronCurrentRunView{
				RunID:     cur.RunID,
				StartedAt: cur.StartedAt.UnixMilli(),
				Phase:     cur.Phase,
				Trigger:   string(cur.Trigger),
				SessionID: cur.SessionID,
			}
		}
		if c := j.RunCounters; c.Total > 0 {
			v.Stats = &cronRunCountersView{
				Total:     c.Total,
				Succeeded: c.Succeeded,
				Failed:    c.Failed,
				Skipped:   c.Skipped,
				TimedOut:  c.TimedOut,
				Canceled:  c.Canceled,
			}
		}
		// recent_runs: P1 — 5 条 newest-first 摘要给卡片 tooltip 用。
		// 上限 5 是 wire 大小的折中：list response 总大小 = jobs × ~2KB。
		// 详情页要更多用 GET /api/cron/runs.
		//
		// R250-PERF-19 (#1122): pre-extend rv to len(recent) and use index
		// assignment in place of append. Skips the per-iteration cap/len
		// bookkeeping the append builtin pays even when the backing array
		// is already pre-sized — a 1Hz × N-tab × 50-job poll churns enough
		// of these short slices that the saved bound checks add up.
		//
		// R236-PERF-08 (#525): the per-job RecentRuns lookup now lives in
		// the bounded-fan-out pass above (batchRecentRuns). Read from the
		// pre-fetched slice indexed by jobs[] position so this loop stays
		// pure projection — no per-iter scheduler call, no per-iter mutex
		// acquire — and the worst-case wall time is bounded by ⌈N/W⌉ × per-
		// job lock cost rather than N × per-job lock cost.
		if recent := recentByIdx[idx]; len(recent) > 0 {
			rv := make([]cronRunSummaryView, len(recent))
			for i, r := range recent {
				rv[i] = cronSummaryToView(r)
			}
			v.RecentRuns = rv
		}
		views = append(views, v)
	}

	loc := h.scheduler.Location()
	// R246-PERF-17: reuse the `now` captured before the job loop so the
	// timezone label is computed at the same moment as the missed-schedule
	// check above. Calling time.Now() a second time here also wasted a
	// syscall on every list request.
	name, offset := now.In(loc).Zone()
	locName := loc.String()
	tzLabel := formatTZOffset(locName, offset)

	// R230B-CR-3: named struct in place of map[string]any keeps the json
	// encoder on the cached reflect path (one-time alloc) and lets the wire
	// shape be grepped directly from the type definition.
	resp := cronListResp{
		Jobs:          views,
		Timezone:      locName,
		TimezoneLabel: tzLabel,
		TimezoneAbbr:  name,
	}
	if def := h.scheduler.NotifyDefault(); def.IsSet() {
		// Expose the configured default so the UI can render helpful copy
		// like "notifications go to feishu (oc_xxx)" instead of just a
		// blank toggle. chat_id is already considered semi-public (appears
		// in message metadata) so surfacing it here is not a leak.
		resp.NotifyDefault = &cronNotifyDefaultView{
			Platform: def.Platform,
			ChatID:   def.ChatID,
		}
	}
	writeJSON(w, resp)
}

// httpErrPersistFailed writes the standard 500 body for the "in-memory
// mutation succeeded but on-disk persist failed" case. The five cron
// write handlers (create / delete / pause / resume / update) all surface
// cron.ErrPersistFailed identically — same status, same wording with
// only the verb differing — so the literal had drifted across five
// copy-paste sites. Centralising the format keeps the wording in one
// place and stops a future copy from accidentally diverging the
// operator-visible string. R250-CR-20.
func httpErrPersistFailed(w http.ResponseWriter, op string) {
	http.Error(w, "job "+op+" but not persisted; please check server logs", http.StatusInternalServerError)
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
		// Backend pins the CLI backend for this job ("" = router default).
		// Per docs/rfc/multi-backend.md §9 cron RPC contract. Validated
		// by validateCronBackend to match the send.go shape contract.
		Backend string `json:"backend,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64 KB
	if err := decodeJSONBody(r, &req); err != nil {
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
	if err := validateCronBackend(req.Backend); err != nil {
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
			status, msg := classifyWorkspaceErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, msg, status)
			return
		}
		req.WorkDir = validated
	}

	// Guard: notify=true without any target (neither per-job override nor
	// scheduler default) would silently swallow notifications. Reject it
	// at the edge so users see the problem immediately.
	//
	// R242-SEC-11: a per-job override is only "set" when BOTH platform and
	// chat_id are present — half-configured (one filled, one blank) used to
	// quietly fall through to NotifyDefault, hiding what is almost always a
	// dashboard form-fill mistake (typo'd ChatID, lost focus before saving
	// platform). Reject the half-set case explicitly with a distinct error
	// so the user can self-correct, instead of letting it land on cron job
	// disk and silently route notifications to the global fallback target.
	if req.NotifyPlatform != "" || req.NotifyChatID != "" {
		if req.NotifyPlatform == "" || req.NotifyChatID == "" {
			http.Error(w, "notify_platform and notify_chat_id must be set together", http.StatusBadRequest)
			return
		}
	}
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
		Backend:        req.Backend,
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
			httpErrPersistFailed(w, "created")
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
	writeJSON(w, cronCreateResp{ID: job.ID})
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
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog: keeps log
	// attributes free of newlines/control bytes that would inject forged
	// records into the operator log when the lookup misses.
	if !cron.IsValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
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
			httpErrPersistFailed(w, "deleted")
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
	if err := decodeJSONBody(r, &req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	// Mirror handleDelete's guard so oversized IDs don't drag slog attrs up
	// to KB-scale strings on failure/success paths. R64-SEC-1.
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
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
			httpErrPersistFailed(w, "paused")
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
	if err := decodeJSONBody(r, &req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		http.Error(w, "id too long", http.StatusBadRequest)
		return
	}
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
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
			httpErrPersistFailed(w, "resumed")
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
	// [R247-SEC-2] Per-IP rate limit: each call spawns the cron job's
	// claude CLI subprocess and may emit IM notifications; without this
	// gate a stolen dashboard token could loop-trigger jobs to amplify
	// CPU/IM-quota damage. Nil-guarded for hand-built test handlers.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := decodeJSONBody(r, &req); err != nil {
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
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(req.ID) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.scheduler.TriggerNow(req.ID); err != nil {
		switch {
		case errors.Is(err, cron.ErrJobNotFound):
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrJobPaused):
			http.Error(w, "job is paused", http.StatusConflict)
		case errors.Is(err, cron.ErrJobNoPrompt):
			http.Error(w, "job has no prompt", http.StatusUnprocessableEntity)
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
	// [R247-SEC-3] Per-IP rate limit. Although preview is read-only and
	// cheaper than trigger, it is not a heartbeat endpoint — each call
	// runs the cron parser plus N=1..10 next-run computations, so an
	// unbounded loop from a stolen token still burns CPU. Share the cron
	// write/control bucket to keep the per-IP control surface uniform.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	schedule := r.URL.Query().Get("schedule")
	if schedule == "" {
		http.Error(w, "schedule is required", http.StatusBadRequest)
		return
	}
	// Cap schedule length so the cron parser (regex + split) cannot be DoS'd
	// with a megabyte-scale query parameter. Real cron expressions are far
	// below this limit; robfig/cron rejects extremely long descriptors anyway.
	if len(schedule) > maxCronScheduleBytesDashboard {
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

	// PreviewScheduleN / Location are nil-receiver-safe (R219-CR-6); the
	// nil path computes in UTC for tests / dashboard bootstrap before the
	// scheduler is wired, matching the behaviour of the deleted
	// cron.PreviewSchedule package-level helper.
	runs, err := h.scheduler.PreviewScheduleN(schedule, count)
	loc := h.scheduler.Location()
	tzName := loc.String()
	tzLabel := ""
	if n, offset := time.Now().In(loc).Zone(); n != "" {
		tzLabel = formatTZOffset(tzName, offset)
	}
	if err != nil {
		// Don't echo the raw robfig/cron parser error: it leaks field offsets
		// and internal token names that help an attacker enumerate accepted
		// grammar. Log the detail for operators instead.
		slog.Debug("cron preview parse failed", "err", err)
		writeJSON(w, cronPreviewResp{Valid: false, Error: "invalid schedule expression"})
		return
	}

	resp := cronPreviewResp{
		Valid:         true,
		Timezone:      tzName,
		TimezoneLabel: tzLabel, // omitempty drops the empty-zone case
	}
	if len(runs) > 0 {
		resp.NextRun = runs[0].UnixMilli()
		nextRuns := make([]int64, len(runs))
		for i, t := range runs {
			nextRuns[i] = t.UnixMilli()
		}
		resp.NextRuns = nextRuns
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
	// [R250-SEC-1] Shape gate before id reaches scheduler/slog.
	if !cron.IsValidID(id) {
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
	if err := decodeJSONBody(r, &req); err != nil {
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
		if err := validateCronBackend(*req.Backend); err != nil {
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
			status, msg := classifyWorkspaceErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			http.Error(w, msg, status)
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

	j, err := h.scheduler.UpdateJob(id, cron.JobUpdate{
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
		case errors.Is(err, cron.ErrJobNotFound):
			// Fixed string (not err.Error()) to stay consistent with
			// handleDelete and guard against future ErrJobNotFound variants
			// that carry a wrapped ID.
			http.Error(w, "job not found", http.StatusNotFound)
		case errors.Is(err, cron.ErrPersistFailed):
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
	writeJSON(w, cronUpdateResp{Status: "ok", ID: j.ID})
}

// formatTZOffset moved to dashboard_cron_view.go (#1281).

// runIDLenLimit caps the run_id query parameter length. Run IDs and job
// IDs share the same generator class (hex with headroom for future
// entropy bumps) and the same on-disk JSON store, so they share the
// cron.MaxIDLen constant. R230B-CR-1: previously a separate const was
// kept "in case of divergence", but no concrete plan exists for run IDs
// to grow longer than job IDs, and two parallel constants drifted in
// review with no source of truth. Reuse one constant; revisit if a real
// divergence requirement appears.
const runIDLenLimit = cron.MaxIDLen

// GET /api/cron/runs?job_id=&limit=&before=
//
// Returns CronRun summaries for one job, newest first. limit default 50,
// clamped to [1, cron.DefaultRunsKeepCount]. before is unix-ms; only runs
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
// (mirrors handleList's policy).
func (h *CronHandlers) handleRunsList(w http.ResponseWriter, r *http.Request) {
	// R222-SEC-3: gate per-IP before any scheduler / FS work so an attacker
	// holding a stolen dashboard token cannot enumerate the run history at
	// unbounded rate. Nil-guarded for hand-built tests.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		// R230B-CR-3: same byte shape as the map[string]any fast path —
		// `{"runs":[]}`. Explicit empty slice keeps json.Marshal off the
		// nil → "null" rendering branch.
		writeJSON(w, cronRunsListResp{Runs: []cronRunSummaryView{}})
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
	if !cron.IsValidID(jobID) {
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
		if n > cron.DefaultRunsKeepCount {
			n = cron.DefaultRunsKeepCount
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
	writeJSON(w, resp)
}

// GET /api/cron/runs/{run_id}?job_id=...
//
// Returns the full CronRun (Prompt + Result + ErrorMsg). 404 when missing,
// 500 with "corrupt record" message when the file exists but fails to
// parse / exceeds size cap. Used by the dashboard detail drawer.
func (h *CronHandlers) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	// R222-SEC-3: same per-IP gate as handleRunsList. Detail reads also do
	// FS I/O (read JSON file from disk) so they share the limiter and
	// budget, not separate buckets — a single bucket prevents bypass-via-
	// alternate-endpoint when both URLs share an identical token.
	if h.runsLimiter != nil && !h.runsLimiter.AllowRequest(r) {
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron runs rate limit exceeded"})
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
	if !cron.IsValidID(runID) {
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
	if !cron.IsValidID(jobID) {
		http.Error(w, "job_id must be lowercase hex", http.StatusBadRequest)
		return
	}
	run, err := h.scheduler.GetRun(jobID, runID)
	if err != nil {
		if errors.Is(err, cron.ErrCorruptRun) {
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
	out := cronRunDetailView{
		RunID:       run.RunID,
		JobID:       run.JobID,
		State:       string(run.State),
		Trigger:     string(run.Trigger),
		StartedAt:   run.StartedAt.UnixMilli(),
		DurationMS:  run.DurationMS,
		SessionID:   run.SessionID,
		Prompt:      osutil.SanitizeForLog(run.Prompt, cron.MaxPromptBytes),
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
	writeJSON(w, out)
}
