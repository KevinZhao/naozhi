package cron

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Notify target bounds: platform must be a known IM provider (misspelt names
// would silently drop notifications); chat_id length is capped so one request
// cannot bloat cron_jobs.json.
var validNotifyPlatforms = map[string]struct{}{
	"":        {}, // empty = fall back to cron.notify_default
	"feishu":  {},
	"slack":   {},
	"discord": {},
	"weixin":  {},
}

const maxNotifyChatIDLen = 256

// Cron input bounds shared with the IM `/cron` path. Both surfaces feed the
// same on-disk cron_jobs.json schema, so the limits must stay in lockstep —
// see internal/cron/limits.go.
const (
	maxCronPromptBytesDashboard   = cronpkg.MaxPromptBytes
	maxCronIDLenDashboard         = cronpkg.MaxIDLen
	maxCronScheduleBytesDashboard = cronpkg.MaxScheduleBytes
)

// maxCronWorkDirBytesDashboard caps raw work_dir before validateWorkspace so a
// multi-MB body is not echoed into slog attrs on failure (log-flood).
const maxCronWorkDirBytesDashboard = 1024

// stringFieldPolicy carries the per-field knobs for validateStringField so every
// cron-edge field shares one UTF-8 + C0 + IsLogInjectionRune scan.
type stringFieldPolicy struct {
	name string
	// allowTab whitelists 0x09 (cron prompt / title body).
	allowTab bool
	// allowLF whitelists 0x0a (cron prompt only; schedules / paths never contain LF).
	allowLF bool
	// disallowLF reports LF / CR as "<name> must be a single line" (Job.Title).
	// Mutually exclusive with allowLF; if both are set allowLF wins.
	disallowLF bool
	// collapseErrors folds every failure class into "<name> contains invalid
	// characters"; work_dir / prompt keep the three-tier messages for audit signal.
	collapseErrors bool
}

// validateStringField runs the UTF-8 → C0+DEL → log-injection rune scan every
// user-controlled cron string requires; callers own the length check and
// field-specific extras. Every IsLogInjectionRune hit has a byte >= 0x80, so a
// provably-ASCII input skips the rune walk (#1125).
func validateStringField(s string, p stringFieldPolicy) error {
	// Validate UTF-8 first: ranging over broken UTF-8 yields U+FFFD, which
	// IsLogInjectionRune does not flag, letting lone continuation bytes smuggle
	// arbitrary bytes into cron_jobs.json / WS broadcasts.
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s contains invalid characters", p.name)
	}
	anyHighBit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			anyHighBit = true
			continue
		}
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c == '\t' && p.allowTab {
			continue
		}
		if c == '\n' && p.allowLF {
			continue
		}
		if p.disallowLF && (c == '\n' || c == '\r') {
			return fmt.Errorf("%s must be a single line", p.name)
		}
		if p.collapseErrors {
			return fmt.Errorf("%s contains invalid characters", p.name)
		}
		return fmt.Errorf("%s contains invalid control characters", p.name)
	}
	if !anyHighBit {
		return nil
	}
	// Reject bidi overrides / isolates (U+202A–U+202E, U+2066–U+2069) and LS/PS
	// (U+2028/U+2029): valid UTF-8 with all bytes >= 0x20, so they pass the byte
	// loop, yet they flip terminal rendering and break log pipelines.
	for _, r := range s {
		if osutil.IsLogInjectionRune(r) {
			if p.collapseErrors {
				return fmt.Errorf("%s contains invalid characters", p.name)
			}
			return fmt.Errorf("%s contains invalid unicode control characters", p.name)
		}
	}
	return nil
}

// validateCronWorkDir rejects oversized / control-character work_dir strings at
// the handler edge (log injection) and relative paths as defense-in-depth: the
// scheduler worker runs on absolute paths only.
func validateCronWorkDir(wd string) error {
	if len(wd) > maxCronWorkDirBytesDashboard {
		return fmt.Errorf("work_dir exceeds %d-byte limit", maxCronWorkDirBytesDashboard)
	}
	if err := validateStringField(wd, stringFieldPolicy{name: "work_dir"}); err != nil {
		return err
	}
	if !filepath.IsAbs(wd) {
		return fmt.Errorf("work_dir must be an absolute path")
	}
	return nil
}

// validateNotifyTarget enforces the platform allowlist, chat_id size bound and
// log-injection rune scan (chat_id lands in cron_jobs.json and WS broadcasts).
func validateNotifyTarget(platform, chatID string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	if len(chatID) > maxNotifyChatIDLen {
		return fmt.Errorf("notify_chat_id exceeds %d-byte limit", maxNotifyChatIDLen)
	}
	return validateStringField(chatID, stringFieldPolicy{name: "notify_chat_id", collapseErrors: true})
}

// validateCronScheduleChars rejects log-injection runes before the schedule
// reaches robfig/cron, whose parser errors are forwarded into operator logs.
func validateCronScheduleChars(schedule string) error {
	// Shared with the IM dispatch.ParseCronAdd edge so policies cannot drift (#1315).
	return cronpkg.ValidateScheduleChars(schedule)
}

// cronRunSummaryView is the JSON shape for one cron run summary, shared by
// HandleList (recent-run preview) and HandleRunsList (paginated history).
type cronRunSummaryView struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	Trigger    string `json:"trigger,omitempty"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	// ReplayOf links a replay run to its origin (agentcore §7.3 chain badge).
	ReplayOf string `json:"replay_of,omitempty"`
	// CostUSD: per-run sandbox cost (§7.5); the front end sums it across recent_runs.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// cronSummaryToView projects a cronpkg.CronRunSummary into cronRunSummaryView.
func cronSummaryToView(r cronpkg.CronRunSummary) cronRunSummaryView {
	row := cronRunSummaryView{
		RunID:      r.RunID,
		State:      string(r.State),
		Trigger:    string(r.Trigger),
		StartedAt:  r.StartedAt.UnixMilli(),
		DurationMS: r.DurationMS,
		SessionID:  osutil.SanitizeForLog(r.SessionID, 64),
		ErrorClass: string(r.ErrorClass),
		ReplayOf:   r.ReplayOf,
		CostUSD:    r.CostUSD,
	}
	if !r.EndedAt.IsZero() {
		row.EndedAt = r.EndedAt.UnixMilli()
	}
	return row
}

// cronCreateResp is the wire shape returned by POST /api/cron; dashboard.js
// cronCreateJob reads only resp.id.
type cronCreateResp struct {
	ID string `json:"id"`
}

// cronCurrentRunView is the in-flight run summary embedded in cronJobView.
type cronCurrentRunView struct {
	RunID     string `json:"run_id"`
	StartedAt int64  `json:"started_at"`
	Phase     string `json:"phase,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// cronRunCountersView mirrors cronpkg.RunCounters (same field order).
type cronRunCountersView struct {
	Total     int64 `json:"total,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Skipped   int64 `json:"skipped,omitempty"`
	TimedOut  int64 `json:"timed_out,omitempty"`
	Canceled  int64 `json:"canceled,omitempty"`
}

// cronRunDetailView is the JSON shape returned by GET /api/cron/{job}/runs/{run}.
type cronRunDetailView struct {
	RunID       string `json:"run_id"`
	JobID       string `json:"job_id"`
	State       string `json:"state"`
	Trigger     string `json:"trigger,omitempty"`
	StartedAt   int64  `json:"started_at"`
	EndedAt     int64  `json:"ended_at,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	WorkDir     string `json:"work_dir,omitempty"`
	Fresh       bool   `json:"fresh,omitempty"`
	Result      string `json:"result,omitempty"`
	ResultBytes int    `json:"result_bytes,omitempty"`
	ErrorClass  string `json:"error_class,omitempty"`
	ErrorMsg    string `json:"error_msg,omitempty"`
	// ReplayOf links a replay run to its origin (§7.3 chain badge).
	ReplayOf string `json:"replay_of,omitempty"`
	// Sandbox is the cloud-execution receipt (RFC §7.3 meta bar), present only
	// for placement=sandbox runs; nil renders no meta bar.
	Sandbox *cronRunSandboxView `json:"sandbox,omitempty"`
}

// cronRunSandboxView is the dashboard projection of cronpkg.SandboxRunMeta (§7.3
// meta bar); a separate type so the cron package stays free of server concerns.
type cronRunSandboxView struct {
	RuntimeARN      string  `json:"runtime_arn,omitempty"`
	ImageVersion    string  `json:"image_version,omitempty"`
	ExitStatus      int     `json:"exit_status"`
	CostUSD         float64 `json:"cost_usd,omitempty"`
	DurationMS      int64   `json:"duration_ms,omitempty"`
	MemoryPeakBytes int64   `json:"memory_peak_bytes,omitempty"`
}

// cronJobView is the per-job element inside cronListResp.Jobs.
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
	// LastErrorClass 是机器可读错误分类。前端用它选图标/色板而非 substring-grep
	// LastError。空 = 无错误 / 旧 job。
	LastErrorClass string `json:"last_error_class,omitempty"`
	NextRun        int64  `json:"next_run,omitempty"`
	// Notify is a pointer to preserve the tri-state; nil renders as "legacy default".
	Notify       *bool `json:"notify,omitempty"`
	FreshContext bool  `json:"fresh_context,omitempty"`
	// Placement 是运行位置（RFC §7.2 徽标数据源）：""/"local" 本机；"sandbox" 云沙箱。
	Placement string `json:"placement,omitempty"`
	// SideEffects 是"有外部副作用"声明（agentcore §6.2）；tri-state 同 Notify。
	SideEffects *bool `json:"side_effects,omitempty"`
	// Missed 表示进程休眠 / 重启空窗期该 job 错过了至少一次调度；MissedSince 是
	// 按 schedule 算上一次应跑的毫秒时刻。未 missed 时两个字段都省略。
	Missed      bool  `json:"missed,omitempty"`
	MissedSince int64 `json:"missed_since,omitempty"`
	// CurrentRun: 仅 job 正在执行时存在，前端据此显示"运行中 Xs"。
	CurrentRun *cronCurrentRunView `json:"current_run,omitempty"`
	// Stats: 累计执行计数；引入 avg_ms / p95_ms 时不动 wire shape。
	Stats *cronRunCountersView `json:"stats,omitempty"`
	// RecentRuns: newest-first 摘要数组，卡片 tooltip 用；空 = 尚无持久化历史。
	RecentRuns []cronRunSummaryView `json:"recent_runs,omitempty"`
	// Backend: "" 表示跟随 router default（docs/rfc/multi-backend.md §9）。
	Backend string `json:"backend,omitempty"`
	// PromptTruncated is set by GET /api/cron?compact=1 when Prompt was clipped;
	// the dashboard refetches the full prompt before opening the editor (#494).
	PromptTruncated bool `json:"prompt_truncated,omitempty"`
}

// compactPromptPrefixBytes bounds Prompt bytes for GET /api/cron?compact=1:
// 50 jobs × 256 B = 12 KiB per poll instead of 400 KiB worst case (#494).
// HandleList clips on a UTF-8 rune boundary so consumers never see a half rune.
const compactPromptPrefixBytes = 256

// truncatePromptUTF8 clips prompt to at most max bytes on a UTF-8 rune boundary
// and reports whether truncation occurred.
func truncatePromptUTF8(prompt string, max int) (string, bool) {
	if max <= 0 || len(prompt) <= max {
		return prompt, false
	}
	// Walk back to a leading UTF-8 byte; bounded by ≤4 steps.
	for n := max; n > 0; n-- {
		if utf8.RuneStart(prompt[n]) {
			return prompt[:n], true
		}
	}
	// Unreachable for valid UTF-8; fall back rather than return invalid bytes.
	return "", true
}

// cronNotifyDefaultView is the {platform, chat_id} pair for cron.notify_default.
type cronNotifyDefaultView struct {
	Platform string `json:"platform"`
	ChatID   string `json:"chat_id"`
}

// maskNotifyChatID redacts the cron.notify_default chat_id before it reaches the
// list response: in a multi-operator deployment the raw value must not leak to
// every authenticated user. Keeps a 4+4 rune hint; IDs <= 8 runes are fully masked (#789).
func maskNotifyChatID(id string) string {
	if id == "" {
		return ""
	}
	r := []rune(id)
	if len(r) <= 8 {
		return strings.Repeat("•", len(r))
	}
	return string(r[:4]) + "…" + string(r[len(r)-4:])
}

// cronRunsListResp is the wire shape returned by GET /api/cron/runs.
type cronRunsListResp struct {
	Runs       []cronRunSummaryView `json:"runs"`
	NextBefore int64                `json:"next_before,omitempty"`
}

// cronPreviewResp is the wire shape returned by GET /api/cron/preview; only
// Error is set when Valid is false.
type cronPreviewResp struct {
	Valid         bool    `json:"valid"`
	Error         string  `json:"error,omitempty"`
	Timezone      string  `json:"timezone,omitempty"`
	TimezoneLabel string  `json:"timezone_label,omitempty"`
	NextRun       int64   `json:"next_run,omitempty"`
	NextRuns      []int64 `json:"next_runs,omitempty"`
}

// cronUpdateResp is the wire shape returned by PATCH /api/cron.
type cronUpdateResp struct {
	Status string `json:"status"`
	ID     string `json:"id"`
}

// ValidateCronBackend enforces the shape contract for the dashboard-picked
// backend override: empty OK, length <= maxBackendIDLen, charset per
// isValidBackendID (shared with the WS path). Unknown backend IDs are NOT
// rejected — the router's wrapperFor clamps them to the default so a job keeps
// running after an operator removes a backend from config.yaml.
func ValidateCronBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendIDLen {
		return fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
	}
	if !isValidBackendID(backend) {
		// Same error string as send.handleSend's gate.
		return fmt.Errorf("invalid backend identifier")
	}
	return nil
}

// validateCronPlacement gates placement at the HTTP edge: value shape
// (""/"local"/"sandbox") and the Phase 1 sandbox guardrail (no work_dir until
// clone-on-boot lands, RFC §4.4 B10-a). The scheduler re-validates; this copy
// gives the dashboard a precise 400 instead of a generic save error.
func validateCronPlacement(placement, workDir string) error {
	switch placement {
	case "", cronpkg.PlacementLocal:
		return nil
	case cronpkg.PlacementSandbox:
		if workDir != "" {
			return fmt.Errorf("云沙箱暂不支持工作目录（Phase 1）：请清空 work_dir 或改用本机运行")
		}
		return nil
	default:
		return fmt.Errorf("invalid placement %q", placement)
	}
}

// validateCronTitle 是 Job.Title 在 handler 层的守门：单行、长度 256 rune、禁控制
// 字符 + 日志注入 rune；空值合法（UI fallback 到 Prompt 首行）。
// 通过 stringFieldPolicy{disallowLF: true} 复用 validateStringField。
func validateCronTitle(title string) error {
	if title == "" {
		return nil
	}
	if n := utf8.RuneCountInString(title); n > cronpkg.MaxCronTitleLen {
		return fmt.Errorf("title exceeds %d-rune limit", cronpkg.MaxCronTitleLen)
	}
	return validateStringField(title, stringFieldPolicy{name: "title", allowTab: true, disallowLF: true})
}

// validateCronPrompt delegates the shared scan to cronpkg.ValidatePromptStrict
// (one policy with the IM `/cron` edge, #1188) and adds one dashboard-only
// rule: reject CR. LF is safe (JSON-quoted in cron_jobs.json) but a bare CR
// survives the encode and carriage-returns over the previous line in
// `tail -f` / `journalctl` — a log-poisoning surface.
func validateCronPrompt(prompt string) error {
	if err := cronpkg.ValidatePromptStrict(prompt); err != nil {
		return err
	}
	if strings.ContainsRune(prompt, '\r') {
		return fmt.Errorf("prompt contains invalid control characters")
	}
	return nil
}

// Handlers groups the cron job management API endpoints.
type Handlers struct {
	scheduler   *cronpkg.Scheduler
	allowedRoot string
	// claudeDir is the absolute path to ~/.claude, used by HandleRunTranscript
	// to locate a run's JSONL. Empty disables the endpoint (fallback:"missing").
	claudeDir string
	// runsLimiter caps per-IP rate of /api/cron/runs and /runs/{run_id}; both
	// fan out filesystem I/O, so a stolen token could otherwise enumerate the
	// run history at unbounded rate. Nil disables the gate (test fixtures).
	runsLimiter IPLimiter

	// listLimiter caps GET /api/cron (1 Hz poll; cost grows with N jobs ×
	// RecentRuns(5)). 2 req/s with burst 30 absorbs a tab refresh storm while
	// capping a parallel-poll attacker per source IP. Nil disables the gate.
	listLimiter IPLimiter

	// transcriptLimiter gives the transcript endpoint its own per-IP budget
	// (#1096): it is far more expensive than runs list/detail, so sharing
	// runsLimiter let either side starve the other into 429. Nil disables the gate.
	transcriptLimiter IPLimiter

	// writeLimiter caps per-IP rate of cron write/control endpoints: trigger
	// spawns the job's claude CLI subprocess and may send IM notifications
	// (loop-trigger amplification); preview runs the parser up to 10 times.
	// 30 req/min with burst 6. Nil disables the gate.
	writeLimiter IPLimiter

	// missedCache memoises HasMissedSchedule verdicts so the 1 Hz poll does not
	// re-Parse every job's cron expression per poll × tab. Keyed by (jobID,
	// schedule, startedAt) so edits / restarts invalidate by key turnover; a
	// LastRunAt advance invalidates via the lastRunNanos guard (#857).
	missedCacheMu sync.RWMutex
	missedCache   map[string]missedVerdict

	// tzLabelMu guards the memoised timezone label. Keyed on (locName, offset),
	// NOT loc.String() alone: a fixed *time.Location's offset still flips
	// across DST transitions.
	tzLabelMu     sync.RWMutex
	tzLabelLoc    string
	tzLabelOffset int
	tzLabelCached string
	tzLabelHasVal bool

	// transcriptSem is a process-wide cap on concurrent transcript requests
	// (#798): each holds a 256 KB scanner buffer plus an 8 MB read budget, so the
	// per-IP limiter alone lets N operators park N×8 MB. Excess requests get 503.
	transcriptSem chan struct{}

	// validateWS / classifyWSErr inject internal/server's validateWorkspace +
	// classifyWorkspaceErr without reverse-importing server. Both nil-safe.
	validateWS    func(ws, root string) (string, error)
	classifyWSErr func(err error) (int, string)
}

// missedVerdict caches one HasMissedSchedule tuple. Keyed by
// `jobID|schedule|startedNs` so schedule edits / restarts invalidate by key
// turnover; lastRunNanos in the value forces a recompute after a fresh run (#857).
type missedVerdict struct {
	missed       bool
	prevAt       time.Time
	lastRunNanos int64
	computedAt   time.Time
}

// missedCacheTTL matches the dashboard poll cadence: a verdict up to one tick
// stale is what a parallel poller would have just computed anyway (#857).
const missedCacheTTL = time.Second

// missedCacheCap bounds the cache (2500 × ~120 B ≈ 300 KiB) so an UpdateJob
// storm cannot grow it without bound. On overflow the oldest decile is shed
// rather than dropping the whole map, so long-lived jobs stay warm (#1352).
const missedCacheCap = 2500

// missedCacheEvictRatio is the fraction of oldest entries shed on overflow; 10%
// keeps the sort+delete sweep amortised without a second LRU index (#1352).
const missedCacheEvictRatio = 10

// missedScheduleVerdict returns HasMissedSchedule(j, now, startedAt), memoised
// for missedCacheTTL. Safe for concurrent callers (#857).
func (h *Handlers) missedScheduleVerdict(j *cronpkg.Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	startedNs := startedAt.UnixNano()
	key := j.ID + "|" + j.Schedule + "|" + strconv.FormatInt(startedNs, 10)
	lastRunNanos := j.LastRunAt.UnixNano()

	h.missedCacheMu.RLock()
	if h.missedCache != nil {
		if v, ok := h.missedCache[key]; ok {
			if v.lastRunNanos == lastRunNanos && now.Sub(v.computedAt) < missedCacheTTL {
				h.missedCacheMu.RUnlock()
				return v.missed, v.prevAt
			}
		}
	}
	h.missedCacheMu.RUnlock()

	missed, prevAt := cronpkg.HasMissedSchedule(j, now, startedAt)

	h.missedCacheMu.Lock()
	if h.missedCache == nil {
		h.missedCache = make(map[string]missedVerdict, 64)
	} else if len(h.missedCache) >= missedCacheCap {
		// Shed the oldest decile so long-lived entries survive an UpdateJob burst (#1352).
		evictOldestMissedCache(h.missedCache)
	}
	h.missedCache[key] = missedVerdict{
		missed:       missed,
		prevAt:       prevAt,
		lastRunNanos: lastRunNanos,
		computedAt:   now,
	}
	h.missedCacheMu.Unlock()
	return missed, prevAt
}

// evictOldestMissedCache drops the oldest 1/missedCacheEvictRatio of entries by
// computedAt (at least one). Caller holds h.missedCacheMu (#1352).
func evictOldestMissedCache(m map[string]missedVerdict) {
	n := len(m)
	if n == 0 {
		return
	}
	drop := n / missedCacheEvictRatio
	if drop < 1 {
		drop = 1
	}
	if drop > n {
		drop = n
	}
	type kt struct {
		k string
		t time.Time
	}
	entries := make([]kt, 0, n)
	for k, v := range m {
		entries = append(entries, kt{k: k, t: v.computedAt})
	}
	slices.SortFunc(entries, func(a, b kt) int {
		return a.t.Compare(b.t)
	})
	for i := 0; i < drop; i++ {
		delete(m, entries[i].k)
	}
}

// recentRunsPerJob is the per-job RecentRuns cap in HandleList's response
// (tooltip-bound); the runs drawer uses GET /api/cron/runs for pagination.
const recentRunsPerJob = 5

// batchRecentRunsWorkers caps concurrent RecentRuns goroutines in
// batchRecentRuns; above ~16 readers jobLock contention makes speedup negative (#525).
const batchRecentRunsWorkers = 8

// batchRecentRuns fans out scheduler.RecentRuns across at most
// batchRecentRunsWorkers goroutines and returns one result per job in input
// order (nil entries for jobs with no history). Nil-safe on empty input (#525).
func (h *Handlers) batchRecentRuns(jobs []cronpkg.JobWithNextRun, n int) [][]cronpkg.CronRunSummary {
	if len(jobs) == 0 || h.scheduler == nil {
		return nil
	}
	out := make([][]cronpkg.CronRunSummary, len(jobs))
	// Each worker atomically claims the next index so the distribution
	// self-balances without allocating a per-call channel (#1847).
	var next atomic.Int64
	workers := batchRecentRunsWorkers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				idx := int(next.Add(1)) - 1
				if idx >= len(jobs) {
					break
				}
				out[idx] = h.scheduler.RecentRuns(jobs[idx].Job.ID, n)
			}
		}()
	}
	wg.Wait()
	return out
}

// GET /api/cron — list all cron jobs (unscoped, admin view). `?compact=1` clips
// Prompt to compactPromptPrefixBytes and stamps prompt_truncated; the default
// keeps the full prompt for out-of-tree consumers (#494).
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	// Gate per-IP before scheduler/FS work (stolen-token enumeration).
	if h.listLimiter != nil && !h.listLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron list rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		// Explicit empty slice (not nil) so json.Marshal emits `{"jobs":[]}`.
		httputil.WriteJSON(w, cronListResp{Jobs: []cronJobView{}})
		return
	}

	compact := r.URL.Query().Get("compact") == "1"

	jobs := h.scheduler.ListAllJobsWithNextRun()
	// Capture once; each job would otherwise pay time.Now() + an atomic load.
	now := time.Now()
	startedAt := h.scheduler.StartedAt()

	// Pre-fetch RecentRuns with bounded parallelism so the 1 Hz poll does not
	// serialise on the per-job recentCacheEntry.mu chain (#525).
	recentByIdx := h.batchRecentRuns(jobs, recentRunsPerJob)

	// One backing array for every job's RecentRuns view; per-job sub-slices
	// encode as independent JSON arrays, so N allocations become one (#1119).
	totalRecent := 0
	for _, r := range recentByIdx {
		totalRecent += len(r)
	}
	var recentBacking []cronRunSummaryView
	if totalRecent > 0 {
		recentBacking = make([]cronRunSummaryView, totalRecent)
	}
	recentBackingNext := 0

	views := make([]cronJobView, 0, len(jobs))
	for idx, entry := range jobs {
		j := entry.Job
		// compact mode clips Prompt and flags prompt_truncated so the dashboard
		// refetches the full body before opening the editor (#494).
		prompt := j.Prompt
		truncated := false
		if compact {
			prompt, truncated = truncatePromptUTF8(j.Prompt, compactPromptPrefixBytes)
		}
		v := cronJobView{
			ID:              j.ID,
			Schedule:        j.Schedule,
			Prompt:          prompt,
			PromptTruncated: truncated,
			Title:           j.Title,
			Platform:        j.Platform,
			ChatID:          maskNotifyChatID(j.ChatID),
			CreatedBy:       j.CreatedBy,
			CreatedAt:       j.CreatedAt.UnixMilli(),
			Paused:          j.Paused,
			WorkDir:         j.WorkDir,
			NotifyPlatform:  j.NotifyPlatform,
			NotifyChatID:    maskNotifyChatID(j.NotifyChatID),
			LastResult:      j.LastResult,
			LastError:       j.LastError,
			LastErrorClass:  string(j.LastErrorClass),
			Notify:          j.Notify,
			FreshContext:    j.FreshContext,
			Backend:         j.Backend,
			Placement:       j.Placement,
			SideEffects:     j.SideEffects,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if !entry.NextRun.IsZero() {
			v.NextRun = entry.NextRun.UnixMilli()
		}
		// missed-schedule 只对非 paused 的 job 判定（暂停的错过是预期行为）；
		// 走 missedScheduleVerdict 让 Parse 结果在轮询间被缓存 (#857)。
		if !j.Paused {
			if missed, prevAt := h.missedScheduleVerdict(&j, now, startedAt); missed {
				v.Missed = true
				v.MissedSince = prevAt.UnixMilli()
			}
		}
		// CurrentRun 只在 job 正在执行时返回；空 stats 也省略以减少线上 noise。
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
		// recent_runs: 卡片 tooltip 用，上限 recentRunsPerJob；详情页用
		// GET /api/cron/runs。Read from the pre-fetched slice (#525).
		if recent := recentByIdx[idx]; len(recent) > 0 {
			// Sub-slice of recentBacking; the only writer into [start:end] (#1119).
			start := recentBackingNext
			end := start + len(recent)
			rv := recentBacking[start:end:end]
			recentBackingNext = end
			for i, r := range recent {
				rv[i] = cronSummaryToView(r)
			}
			v.RecentRuns = rv
		}
		views = append(views, v)
	}

	loc := h.scheduler.Location()
	// Reuse `now` so the tz label and missed-schedule check share one instant.
	name, offset := now.In(loc).Zone()
	locName := loc.String()
	tzLabel := h.cachedTZLabel(locName, offset)

	resp := cronListResp{
		Jobs:          views,
		Timezone:      locName,
		TimezoneLabel: tzLabel,
		RecentRunsCap: recentRunsPerJob,
		TimezoneAbbr:  name,
	}
	if def := h.scheduler.NotifyDefault(); def.IsSet() {
		// The chat_id is masked: in a multi-operator deployment it is a private
		// notification target and must not leak verbatim to every user (#789).
		resp.NotifyDefault = &cronNotifyDefaultView{
			Platform: def.Platform,
			ChatID:   maskNotifyChatID(def.ChatID),
		}
	}
	httputil.WriteJSON(w, resp)
}

// httpErrPersistFailed writes the 500 errResp envelope for "in-memory mutation
// succeeded but on-disk persist failed", shared by the five cron write handlers.
// Code is the stable "persist_failed" token; the message keeps the verb (#1274).
func httpErrPersistFailed(w http.ResponseWriter, op string) {
	httputil.WriteJSONStatus(w, http.StatusInternalServerError, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{
		Error: "job " + op + " but not persisted; please check server logs",
		Code:  "persist_failed",
	})
}

// writeCronErr writes a cron mutation error as the JSON envelope {"error": msg}
// so dashboard.js reads body.error uniformly (#1518).
func writeCronErr(w http.ResponseWriter, status int, msg string) {
	httputil.WriteJSONStatus(w, status, map[string]string{"error": msg})
}

// POST /api/cron — create a new cron job from dashboard.
func (h *Handlers) HandleCreate(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit: mutations write cron_jobs.json and mutate the scheduler
	// map, so a stolen token must not amplify IO damage. Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
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
		// Backend pins the CLI backend ("" = router default); see ValidateCronBackend.
		Backend string `json:"backend,omitempty"`
		// Placement: ""/"local" = this host, "sandbox" = AgentCore microVM
		// (RFC §4.2); validateCronPlacement gates values and the sandbox guardrails.
		Placement string `json:"placement,omitempty"`
		// SideEffects declares external mutation (agentcore §6.2); nil = off.
		SideEffects *bool `json:"side_effects,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64 KB
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeCronErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Schedule == "" {
		writeCronErr(w, http.StatusBadRequest, "schedule is required")
		return
	}
	if err := validateCronTitle(req.Title); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cap schedule length before the robfig/cron parser: MaxBytesReader caps
	// the body at 64 KB but a 63 KB schedule would still reach the parser.
	if len(req.Schedule) > maxCronScheduleBytesDashboard {
		writeCronErr(w, http.StatusBadRequest, "schedule too long")
		return
	}
	if err := validateCronScheduleChars(req.Schedule); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCronPrompt(req.Prompt); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ValidateCronBackend(req.Backend); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCronPlacement(req.Placement, req.WorkDir); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// work_dir must be under allowedRoot; 403 matches /api/sessions/send so
	// clients see a uniform status for boundary violations.
	if req.WorkDir != "" {
		if err := validateCronWorkDir(req.WorkDir); err != nil {
			writeCronErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if h.validateWS == nil {
			writeCronErr(w, http.StatusInternalServerError, "cron work_dir validation not wired")
			return
		}
		validated, err := h.validateWS(req.WorkDir, h.allowedRoot)
		if err != nil {
			status, msg := h.classifyWSErr(err)
			slog.Debug("cron work_dir validation failed", "err", err)
			writeCronErr(w, status, msg)
			return
		}
		req.WorkDir = validated
	}

	// notify=true without any target (per-job override or scheduler default)
	// would silently swallow notifications. A per-job override counts as "set"
	// only when BOTH fields are present; a half-set pair is a form-fill mistake.
	if req.NotifyPlatform != "" || req.NotifyChatID != "" {
		if req.NotifyPlatform == "" || req.NotifyChatID == "" {
			writeCronErr(w, http.StatusBadRequest, "notify_platform and notify_chat_id must be set together")
			return
		}
	}
	if req.Notify != nil && *req.Notify {
		perJobSet := req.NotifyPlatform != "" && req.NotifyChatID != ""
		if !perJobSet && !h.scheduler.NotifyDefault().IsSet() {
			writeCronErr(w, http.StatusBadRequest, "notify=true but no target configured: set cron.notify_default in config or provide notify_platform/notify_chat_id")
			return
		}
	}

	if err := validateNotifyTarget(req.NotifyPlatform, req.NotifyChatID); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}

	job := &cronpkg.Job{
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
		Placement:      req.Placement,
		SideEffects:    req.SideEffects,
		Paused:         req.Prompt == "", // auto-pause when no prompt
	}
	if err := h.scheduler.AddJob(job); err != nil {
		// ErrPersistFailed: in-memory insert succeeded but the store write
		// failed; surface 500 rather than a 2xx that won't survive a restart.
		if errors.Is(err, cronpkg.ErrPersistFailed) {
			slog.Error("cron AddJob persisted in-memory but store write failed", "err", err, "id", osutil.SanitizeForLog(job.ID, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "created")
			return
		}
		// robfig/cron parser errors leak field offsets / parsed expressions;
		// log the detail for operators, return a sanitized message.
		slog.Warn("cron AddJob rejected", "err", err, "schedule", job.Schedule)
		writeCronErr(w, http.StatusBadRequest, "invalid schedule or job fields")
		return
	}

	slog.Info("cron job created via dashboard", "id", osutil.SanitizeForLog(job.ID, cronpkg.MaxIDLen), "schedule", job.Schedule)
	httputil.WriteJSON(w, cronCreateResp{ID: job.ID})
}

// DELETE /api/cron?id=xxx — delete a cron job by exact ID.
func (h *Handlers) HandleDelete(w http.ResponseWriter, r *http.Request) {
	// Per-IP write rate limit (see HandleCreate). Nil-guarded for tests.
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
	// Reject oversized ids before slog attrs in the error path inflate;
	// maxCronIDLen matches the IM-side guard in dispatch/commands.go.
	if len(id) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog (log forgery on lookup miss).
	if !cronpkg.IsValidID(id) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	j, err := h.scheduler.DeleteJobByID(id)
	if err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			// Deletion already happened in memory; a restart would replay the job.
			slog.Error("cron DeleteJobByID deletion not persisted", "err", err, "id", osutil.SanitizeForLog(id, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "deleted")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron delete failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "delete failed")
		}
		return
	}

	slog.Info("cron job deleted via dashboard", "id", osutil.SanitizeForLog(j.ID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// POST /api/cron/pause — pause a cron job by exact ID.
func (h *Handlers) HandlePause(w http.ResponseWriter, r *http.Request) {
	// Per-IP write rate limit (see HandleCreate). Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.ID == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	// Mirror HandleDelete's guard so oversized IDs don't inflate slog attrs.
	if len(req.ID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(req.ID) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	if _, err := h.scheduler.PauseJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobAlreadyPaused):
			writeCronErr(w, http.StatusConflict, "job already paused")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron PauseJobByID pause not persisted", "err", err, "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "paused")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron pause failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "pause failed")
		}
		return
	}

	slog.Info("cron job paused via dashboard", "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// POST /api/cron/resume — resume a paused cron job by exact ID.
func (h *Handlers) HandleResume(w http.ResponseWriter, r *http.Request) {
	// Per-IP write rate limit (see HandleCreate). Nil-guarded for tests.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.ID == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(req.ID) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	if _, err := h.scheduler.ResumeJobByID(req.ID); err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobNotPaused):
			writeCronErr(w, http.StatusConflict, "job not paused")
		case errors.Is(err, cronpkg.ErrPersistFailed):
			slog.Error("cron ResumeJobByID resume not persisted", "err", err, "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
			httpErrPersistFailed(w, "resumed")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron resume failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "resume failed")
		}
		return
	}

	slog.Info("cron job resumed via dashboard", "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
	httputil.WriteOK(w)
}

// POST /api/cron/trigger — manually trigger a cron job execution (for debugging).
func (h *Handlers) HandleTrigger(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit: each call spawns the job's claude CLI subprocess and
	// may emit IM notifications — a loop-trigger amplification vector.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	if h.scheduler == nil {
		writeCronErr(w, http.StatusNotImplemented, "cron not configured")
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := httputil.DecodeJSONBody(r, &req); err != nil {
		writeCronErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ID == "" {
		writeCronErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if len(req.ID) > maxCronIDLenDashboard {
		writeCronErr(w, http.StatusBadRequest, "id too long")
		return
	}
	// Shape gate before id reaches scheduler/slog.
	if !cronpkg.IsValidID(req.ID) {
		writeCronErr(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.scheduler.TriggerNow(req.ID); err != nil {
		switch {
		case errors.Is(err, cronpkg.ErrJobNotFound):
			writeCronErr(w, http.StatusNotFound, "job not found")
		case errors.Is(err, cronpkg.ErrJobPaused):
			writeCronErr(w, http.StatusConflict, "job is paused")
		case errors.Is(err, cronpkg.ErrJobNoPrompt):
			writeCronErr(w, http.StatusUnprocessableEntity, "job has no prompt")
		default:
			code := cronpkg.ClassifyError(err)
			slog.Debug("cron trigger failed", "err", err)
			writeCronErr(w, code.HTTPStatus(), "trigger failed")
		}
		return
	}

	slog.Info("cron job triggered manually", "id", osutil.SanitizeForLog(req.ID, cronpkg.MaxIDLen))
	httputil.WriteJSON(w, map[string]string{"status": "triggered"})
}

// GET /api/cron/preview?schedule=...&count=N — validate schedule and return the
// next N run times. count defaults to 1 and is clamped to [1, 10].
func (h *Handlers) HandlePreview(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit: parser + up to 10 next-run computations per call.
	if h.writeLimiter != nil && !h.writeLimiter.AllowRequest(r) {
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "cron write rate limit exceeded"})
		return
	}
	schedule := r.URL.Query().Get("schedule")
	if schedule == "" {
		writeCronErr(w, http.StatusBadRequest, "schedule is required")
		return
	}
	// Cap schedule length so the parser cannot be DoS'd with a megabyte query param.
	if len(schedule) > maxCronScheduleBytesDashboard {
		writeCronErr(w, http.StatusBadRequest, "schedule too long")
		return
	}
	if err := validateCronScheduleChars(schedule); err != nil {
		writeCronErr(w, http.StatusBadRequest, err.Error())
		return
	}

	count := 1
	if raw := r.URL.Query().Get("count"); raw != "" {
		// Reject huge inputs before Atoi.
		if len(raw) > 3 {
			writeCronErr(w, http.StatusBadRequest, "count must be a positive integer")
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeCronErr(w, http.StatusBadRequest, "count must be a positive integer")
			return
		}
		if n > 10 {
			n = 10
		}
		count = n
	}

	// PreviewScheduleN / Location are nil-receiver-safe (UTC before wiring).
	runs, err := h.scheduler.PreviewScheduleN(schedule, count)
	loc := h.scheduler.Location()
	tzName := loc.String()
	tzLabel := ""
	if n, offset := time.Now().In(loc).Zone(); n != "" {
		tzLabel = formatTZOffset(tzName, offset)
	}
	if err != nil {
		// Don't echo the raw parser error: field offsets / token names help an
		// attacker enumerate accepted grammar. Log the detail instead.
		slog.Debug("cron preview parse failed", "err", err)
		httputil.WriteJSON(w, cronPreviewResp{Valid: false, Error: "invalid schedule expression"})
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
	httputil.WriteJSON(w, resp)
}

// cachedTZLabel memoises formatTZOffset(locName, offset) for the 1 Hz poll.
// Keyed on offset too because a fixed location's offset flips across DST.
func (h *Handlers) cachedTZLabel(locName string, offset int) string {
	// Fast path: read-lock for the ≈100% cache-hit case (DST flips are rare).
	h.tzLabelMu.RLock()
	if h.tzLabelHasVal && h.tzLabelLoc == locName && h.tzLabelOffset == offset {
		cached := h.tzLabelCached
		h.tzLabelMu.RUnlock()
		return cached
	}
	h.tzLabelMu.RUnlock()

	// Slow path: write-lock with double-check so concurrent misses don't both recompute.
	h.tzLabelMu.Lock()
	defer h.tzLabelMu.Unlock()
	if h.tzLabelHasVal && h.tzLabelLoc == locName && h.tzLabelOffset == offset {
		return h.tzLabelCached
	}
	label := formatTZOffset(locName, offset)
	h.tzLabelLoc = locName
	h.tzLabelOffset = offset
	h.tzLabelCached = label
	h.tzLabelHasVal = true
	return label
}

// formatTZOffset renders a label like "Asia/Shanghai (UTC+08:00)". ianaName is
// the IANA zone identifier, NOT the abbr (sent separately as timezone_abbr).
// The minute component is abs()'d so "UTC-05:30" does not render as "UTC-05:-30".
func formatTZOffset(ianaName string, offsetSeconds int) string {
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	if minutes < 0 {
		minutes = -minutes
	}
	return fmt.Sprintf("%s (UTC%+03d:%02d)", ianaName, hours, minutes)
}

// HasRunsLimiter / HasListLimiter / HasWriteLimiter / HasTranscriptLimiter
// expose limiter nil-state for server's boot-time invariant check.
func (h *Handlers) HasRunsLimiter() bool       { return h.runsLimiter != nil }
func (h *Handlers) HasListLimiter() bool       { return h.listLimiter != nil }
func (h *Handlers) HasWriteLimiter() bool      { return h.writeLimiter != nil }
func (h *Handlers) HasTranscriptLimiter() bool { return h.transcriptLimiter != nil }

// Deps bundles all wiring for New.
type Deps struct {
	Scheduler         *cronpkg.Scheduler
	AllowedRoot       string
	ClaudeDir         string
	RunsLimiter       IPLimiter
	ListLimiter       IPLimiter
	TranscriptLimiter IPLimiter
	WriteLimiter      IPLimiter
	TranscriptSemCap  int
	ValidateWS        func(ws, root string) (string, error)
	ClassifyWSErr     func(err error) (int, string)
}

// New constructs a Handlers from injected deps.
func New(d Deps) *Handlers {
	var sem chan struct{}
	if d.TranscriptSemCap > 0 {
		sem = make(chan struct{}, d.TranscriptSemCap)
	}
	return &Handlers{
		scheduler:         d.Scheduler,
		allowedRoot:       d.AllowedRoot,
		claudeDir:         d.ClaudeDir,
		runsLimiter:       d.RunsLimiter,
		listLimiter:       d.ListLimiter,
		transcriptLimiter: d.TranscriptLimiter,
		writeLimiter:      d.WriteLimiter,
		transcriptSem:     sem,
		validateWS:        d.ValidateWS,
		classifyWSErr:     d.ClassifyWSErr,
	}
}
