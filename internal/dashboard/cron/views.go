package cron

import (
	"strings"
	"unicode/utf8"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
)

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
