package server

import (
	"fmt"

	"github.com/naozhi/naozhi/internal/cron"
)

// This file holds the cron HTTP wire-shape view types and the small
// pure-projection helpers that build them. Extracted from dashboard_cron.go
// (#1281) so the JSON response shapes are reachable in one place — handler
// bodies stay focused on routing/persistence and the wire contract is
// reviewable as a unit. No struct/wireup change.

// cronRunSummaryView is the JSON shape for a single cron run summary.
// Shared between handleList (recent-run preview embedded in the cron
// dashboard list) and handleRunsList (per-job paginated history).
// Both wire shapes must stay in lockstep, so the type is package-level
// rather than declared twice in handler bodies.
type cronRunSummaryView struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	Trigger    string `json:"trigger,omitempty"`
	StartedAt  int64  `json:"started_at"`
	EndedAt    int64  `json:"ended_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
}

// cronSummaryToView projects a cron.CronRunSummary into the wire shape used
// by handleList's recent_runs preview and handleRunsList's paginated history.
// R233-CR-3: the two callers had identical conversion blocks and would
// silently diverge if a future field were added in only one place.
func cronSummaryToView(r cron.CronRunSummary) cronRunSummaryView {
	row := cronRunSummaryView{
		RunID:      r.RunID,
		State:      string(r.State),
		Trigger:    string(r.Trigger),
		StartedAt:  r.StartedAt.UnixMilli(),
		DurationMS: r.DurationMS,
		SessionID:  r.SessionID,
		ErrorClass: string(r.ErrorClass),
	}
	if !r.EndedAt.IsZero() {
		row.EndedAt = r.EndedAt.UnixMilli()
	}
	return row
}

// cronListResp is the wire shape returned by GET /api/cron — the dashboard
// list view. R230B-CR-3 swapped the previous map[string]any literal for
// this named struct so the JSON encoder can cache the type's reflect
// descriptor across the 1-Hz dashboard polls instead of paying the
// per-call map iteration + interface boxing each request.
type cronListResp struct {
	Jobs          []cronJobView          `json:"jobs"`
	Timezone      string                 `json:"timezone"`
	TimezoneLabel string                 `json:"timezone_label"`
	TimezoneAbbr  string                 `json:"timezone_abbr"`
	NotifyDefault *cronNotifyDefaultView `json:"notify_default,omitempty"`
}

// cronCreateResp is the wire shape returned by POST /api/cron — just the
// new job ID. R242-CR-10 promoted this from an inline map[string]any so
// the JSON encoder can cache the reflect descriptor and the field name
// is enforced at compile time.
//
// Wire-only consumer: dashboard.js cronCreateJob reads only resp.id;
// adding fields requires updating the JS reader as well.
type cronCreateResp struct {
	ID string `json:"id"`
}

// cronCurrentRunView is the inline running-run summary embedded in
// cronJobView. Only set when the scheduler reports an in-flight run for
// the corresponding job at list time.
type cronCurrentRunView struct {
	RunID     string `json:"run_id"`
	StartedAt int64  `json:"started_at"`
	Phase     string `json:"phase,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// cronRunCountersView mirrors cron.RunCounters for the dashboard. Field
// order tracks RunCounters so JSON wire keys cannot diverge silently.
type cronRunCountersView struct {
	Total     int64 `json:"total,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Skipped   int64 `json:"skipped,omitempty"`
	TimedOut  int64 `json:"timed_out,omitempty"`
	Canceled  int64 `json:"canceled,omitempty"`
}

// cronRunDetailView is the JSON shape returned by GET /api/cron/{job}/runs/{run}.
// Promoted from a handler-body anonymous struct (R239-CR-9) for parity with
// cronRunSummaryView / cronJobView / cronRunCountersView — a package-level
// type can be referenced from tests and any future helper that constructs
// the same payload, instead of being trapped inside the handler closure.
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
}

// cronJobView is the per-job element inside cronListResp.Jobs. Promoted to
// package-level (R230B-CR-3) so cronListResp can reference it; the previous
// inline-typed declaration kept it locked inside handleList.
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
	// LastErrorClass 是 P0 cron-run-history 的机器可读错误分类。前端用它
	// 选图标/色板而非 substring-grep LastError。空 = 无错误 / 旧 job。
	LastErrorClass string `json:"last_error_class,omitempty"`
	NextRun        int64  `json:"next_run,omitempty"`
	// Notify is a pointer so the view preserves the tri-state (nil vs
	// explicit true/false). nil renders as "legacy default" on the client.
	Notify       *bool `json:"notify,omitempty"`
	FreshContext bool  `json:"fresh_context,omitempty"`
	// Missed / MissedSince: cron-v2-polish §3.3 Increment C.
	// missed=true 表示进程休眠 / 重启空窗期该 job 错过了至少一次调度。
	// MissedSince 是"按 schedule 算上一次应跑的毫秒时刻"，UI 可以用来
	// 显示 "上次应跑于 …"。未 missed 时两个字段都省略。
	Missed      bool  `json:"missed,omitempty"`
	MissedSince int64 `json:"missed_since,omitempty"`
	// CurrentRun: P0 — 仅 job 正在执行时存在，前端据此显示"运行中 Xs"。
	CurrentRun *cronCurrentRunView `json:"current_run,omitempty"`
	// Stats: P0 — 累计执行计数；P1 引入 avg_ms / p95_ms 时不动 wire shape。
	Stats *cronRunCountersView `json:"stats,omitempty"`
	// RecentRuns: P1 cron-run-history — newest-first 摘要数组；只在卡片
	// 折叠态做 hover-tooltip 状态气泡。空 = 此 job 尚无持久化历史
	// （新建 / 历史已被 GC 清空 / StorePath 为空）。
	RecentRuns []cronRunSummaryView `json:"recent_runs,omitempty"`
	// Backend: per docs/rfc/multi-backend.md §9 cron RPC contract. ""
	// 表示跟随 router default；前端编辑器据此回填 backend 下拉选项。
	Backend string `json:"backend,omitempty"`
}

// cronNotifyDefaultView mirrors the {platform, chat_id} pair previously
// rendered as a nested map[string]string. Pointer-typed in cronListResp so
// the omitempty actually drops the key when no default is configured.
type cronNotifyDefaultView struct {
	Platform string `json:"platform"`
	ChatID   string `json:"chat_id"`
}

// cronRunsListResp is the wire shape returned by GET /api/cron/runs —
// per-job paginated history. NextBefore is omitted when the page is
// partial, matching the previous map[string]any behaviour.
type cronRunsListResp struct {
	Runs       []cronRunSummaryView `json:"runs"`
	NextBefore int64                `json:"next_before,omitempty"`
}

// cronPreviewResp is the wire shape returned by GET /api/cron/preview.
// Replaces an inline map[string]any so the JSON encoder can cache the
// reflect descriptor and field names are enforced at compile time
// (R250-CR-25). The "valid: false" branch carries only Error; the
// success branch carries Timezone (+ optional TimezoneLabel) and, when
// the parser produced runs, NextRun and NextRuns. omitempty keeps the
// on-the-wire shape identical to the previous map[string]any output —
// dashboard.js cronPreviewJob still reads `valid` / `error` /
// `timezone` / `timezone_label` / `next_run` / `next_runs` unchanged.
type cronPreviewResp struct {
	Valid         bool    `json:"valid"`
	Error         string  `json:"error,omitempty"`
	Timezone      string  `json:"timezone,omitempty"`
	TimezoneLabel string  `json:"timezone_label,omitempty"`
	NextRun       int64   `json:"next_run,omitempty"`
	NextRuns      []int64 `json:"next_runs,omitempty"`
}

// cronUpdateResp is the wire shape returned by PATCH /api/cron. Replaces
// an inline map[string]any so the field names are compile-checked
// (R250-CR-24). Status is always "ok" on the success path; ID echoes the
// canonical job ID resolved by UpdateJob.
type cronUpdateResp struct {
	Status string `json:"status"`
	ID     string `json:"id"`
}

// formatTZOffset renders a timezone label like "Asia/Shanghai (UTC+08:00)" or
// "America/St_Johns (UTC-03:30)". The ianaName parameter is the IANA zone
// identifier (loc.String()), NOT the abbr ("CST"/"NDT"). Both call sites
// surface ianaName here and the abbr in a separate timezone_abbr response
// field; the dashboard renders the IANA prefix because it's unambiguous
// across DST transitions, while abbr is shown alongside as a familiar
// short label. R230B-CR-2: parameter renamed from `name` to make the
// expected input explicit and prevent future call sites from passing the
// abbr by mistake.
//
// The integer-division approach would produce "UTC-05:-30" for fractional
// negative offsets because the sub-hour remainder inherits the sign;
// abs() the minute component to keep the format well-formed.
func formatTZOffset(ianaName string, offsetSeconds int) string {
	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60
	if minutes < 0 {
		minutes = -minutes
	}
	return fmt.Sprintf("%s (UTC%+03d:%02d)", ianaName, hours, minutes)
}
