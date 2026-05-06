package cron

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// Job represents a scheduled cron task.
type Job struct {
	ID        string    `json:"id"`
	Schedule  string    `json:"schedule"`
	Prompt    string    `json:"prompt"`
	Platform  string    `json:"platform"`
	ChatID    string    `json:"chat_id"`
	ChatType  string    `json:"chat_type"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Paused    bool      `json:"paused"`

	// Title 是人类可读的任务名称，用于卡片列表显示、搜索主 key、通知标题。
	// 为空时 UI 自动回退到 Prompt 首行（见 JobTitleOrFallback），保持对旧
	// cron_jobs.json 的向后兼容：JSON 反序列化后 Title == "" 不会破坏任何
	// 渲染/搜索路径。上限 MaxCronTitleLen 字节。
	// 引入背景：docs/rfc/cron-v2-polish.md §3.1。
	Title string `json:"title,omitempty"`

	// Optional working directory override for the CLI process.
	WorkDir string `json:"work_dir,omitempty"`

	// Optional notification target for dashboard-created jobs.
	// When set, execution results are also sent to this IM channel.
	NotifyPlatform string `json:"notify_platform,omitempty"`
	NotifyChatID   string `json:"notify_chat_id,omitempty"`

	// Notify controls whether execution results are pushed to an IM channel
	// after each run. Tri-state pointer so old jobs (nil) preserve legacy
	// behavior: IM-created jobs reply to their source chat; dashboard-created
	// jobs honor per-job NotifyPlatform/NotifyChatID if set.
	// Explicit true/false lets dashboard users toggle delivery using the
	// scheduler's notify_default target (or per-job override) without touching
	// platform/chat fields.
	Notify *bool `json:"notify,omitempty"`

	// FreshContext, when true, resets the cron session before each run so the
	// CLI starts from a clean slate instead of inheriting the conversation
	// history from previous executions. Default (false) preserves the existing
	// behavior — session is long-lived and each run appends a new turn to the
	// accumulated context. Fresh mode keeps per-run latency bounded when the
	// job repeatedly does independent work (reviews, status scans, etc.).
	FreshContext bool `json:"fresh_context,omitempty"`

	// Last execution result, persisted across restarts. LastRunAt has no
	// omitempty: encoding/json does not drop zero-value time.Time structs,
	// so the tag was a lint-only hint that falsely implied zero-value
	// omission. Dashboard code already checks LastRunAt.IsZero() before
	// formatting, which handles the "never run" case.
	LastResult string    `json:"last_result,omitempty"`
	LastRunAt  time.Time `json:"last_run_at"`
	LastError  string    `json:"last_error,omitempty"`

	entryID robfigcron.EntryID // runtime only, not persisted
}

// generateID returns a 16-char hex string (8 bytes of entropy).
func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("%x", b)
}

// MaxCronTitleLen 是 Job.Title 的字符上限（UTF-8 rune 计）。256 覆盖绝大多数
// 人类可读名称，且与 dashboard 的 escAttr 线长相容。导出以便 server 包
// 在 handler 层复用同一上限，避免两处数字不同步漂移。
const MaxCronTitleLen = 256

// titleFallbackRuneLimit 是 Title 为空时 UI/搜索用 Prompt 首行截断的
// 长度上限（按 rune 算，避免切断中文）。60 rune 与卡片视觉宽度对齐。
const titleFallbackRuneLimit = 60

// JobTitleOrFallback 返回用于 UI 显示 / 搜索主 key 的人类可读名称：
//  1. 如果 Job.Title 非空，直接返回（Trim 后）。
//  2. 否则取 Prompt 的首个非空行，截断到 titleFallbackRuneLimit rune。
//  3. 若 Prompt 也为空，返回空字符串——调用方（UI 层）自行决定占位符。
//
// 提供在 cron 包内，供 scheduler / dashboard / IM 通知复用，避免 fallback
// 逻辑散落在各处 UI 代码里时序不一致（dashboard 和后端通知显示的标题
// 出现分歧）。
func JobTitleOrFallback(j *Job) string {
	if j == nil {
		return ""
	}
	if t := strings.TrimSpace(j.Title); t != "" {
		return t
	}
	p := strings.TrimSpace(j.Prompt)
	if p == "" {
		return ""
	}
	// 取首个非空行
	line := p
	if idx := strings.IndexByte(p, '\n'); idx >= 0 {
		line = strings.TrimSpace(p[:idx])
		if line == "" {
			// 罕见：以空白换行开头，继续扫
			for _, l := range strings.Split(p, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					line = l
					break
				}
			}
		}
	}
	// rune-level 截断，保证不切断多字节
	runes := []rune(line)
	if len(runes) > titleFallbackRuneLimit {
		line = string(runes[:titleFallbackRuneLimit]) + "…"
	}
	return line
}

// cronParser is the shared parser for all schedule validation and preview.
var cronParser = robfigcron.NewParser(
	robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor,
)

// minCronInterval is the minimum allowed interval between cron runs.
// Prevents resource exhaustion from overly frequent schedules like "@every 1s".
const minCronInterval = 5 * time.Minute

// jobTimeoutRatio scales a job's schedule period into its execution timeout.
// 0.8 leaves a 20% margin between timeout expiry and the next scheduled tick,
// so a long-running job does not collide with its own next trigger (which the
// SkipIfStillRunning chain and jobRunningGuard would otherwise drop entirely).
const jobTimeoutRatio = 0.8

// minJobTimeout floors the scaled timeout so schedules near minCronInterval
// (5m × 0.8 = 4m) still leave the job a workable budget. 3m matches the
// smallest prompt-roundtrip plus startup shim reconnect observed in prod.
const minJobTimeout = 3 * time.Minute

// computeJobTimeout returns the per-run deadline for a job whose schedule is
// `schedule`. The timeout is period × jobTimeoutRatio, clamped to
// [minJobTimeout, maxCap]. maxCap is the scheduler-level ceiling
// (SchedulerConfig.ExecTimeout) so operators retain a global upper bound.
//
// Clamp order matters: cap is applied last so a caller-configured cap that
// happens to sit below minJobTimeout (pathological / misconfiguration —
// production applies a 5m cap default, above the 3m floor) still wins.
// Without that final cap clamp a 30s-cap caller would get 3m back, which
// would violate the "operators retain a global upper bound" contract.
//
// If schedule is unparseable or the period is non-positive (fixed times, DST
// edge), returns maxCap — safer to fall back to the historical single-timeout
// behaviour than to misapply a ratio to an undefined period.
func computeJobTimeout(schedule string, maxCap time.Duration) time.Duration {
	period := schedulePeriod(schedule)
	if period <= 0 {
		return maxCap
	}
	scaled := time.Duration(float64(period) * jobTimeoutRatio)
	if scaled < minJobTimeout {
		scaled = minJobTimeout
	}
	if scaled > maxCap {
		scaled = maxCap
	}
	return scaled
}

// schedulePeriod 估算给定 cron 表达式的周期（相邻两次触发的间隔）。
// 通过 sched.Next 两次外推实现，精度对 "每 N 分钟 / 每天 HH:MM" 这类
// 常见形态足够。无法解析 / 不等间隔（DST 切换窗口）时返回 0，调用方
// 自行决定 fallback。computeJobTimeout 和 applyJitter 都基于此。
func schedulePeriod(schedule string) time.Duration {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return 0
	}
	now := time.Now()
	first := sched.Next(now)
	second := sched.Next(first)
	return second.Sub(first)
}

// validateSchedule checks if the cron expression is valid and respects the minimum interval.
func validateSchedule(schedule string) error {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return err
	}
	// Check that the interval between the first two runs is at least minCronInterval.
	now := time.Now()
	first := sched.Next(now)
	second := sched.Next(first)
	if interval := second.Sub(first); interval > 0 && interval < minCronInterval {
		return fmt.Errorf("interval %v is too short, minimum is %v", interval, minCronInterval)
	}
	return nil
}
