package cron

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/textutil"
)

// JobIMContext bundles the originating IM-channel coordinates a cron Job
// inherits from the message that created it. Kept as a tiny local type
// (not platform.IncomingMessage) so external callers don't have to import
// the platform package just to construct a Job — and so adding a new IM
// field to platform.IncomingMessage doesn't ripple into Job's wire schema.
//
// All fields are optional: dashboard-created jobs leave the whole struct
// zero-value (the dashboard handler sets dashboard-specific Notify fields
// directly on the returned Job).
type JobIMContext struct {
	Platform  string
	ChatID    string
	ChatType  string
	CreatedBy string
}

// NewJob constructs a Job ready to hand to Scheduler.AddJob from the
// (schedule, prompt) pair plus the IM-channel context that originated it.
// Centralising this constructor prevents cross-package callers (dispatch,
// dashboard, IM command handlers) from spelling out the cron.Job{} struct
// literal — a Job field rename today breaks every literal call site.
//
// CreatedAt is intentionally NOT stamped here: AddJob is the choke point
// that owns Job persistence and needs a single coherent timestamp source.
// Pre-stamping in NewJob would cause a tiny but real skew (LastRunAt
// comparisons, missed-schedule detection) when the constructor is called
// far ahead of AddJob.
func NewJob(schedule, prompt string, ctx JobIMContext) *Job {
	return &Job{
		Schedule:  schedule,
		Prompt:    prompt,
		Platform:  ctx.Platform,
		ChatID:    ctx.ChatID,
		ChatType:  ctx.ChatType,
		CreatedBy: ctx.CreatedBy,
	}
}

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
	// 为空时 UI 自动回退到 Prompt 首行（见 jobTitleOrFallback），保持对旧
	// cron_jobs.json 的向后兼容：JSON 反序列化后 Title == "" 不会破坏任何
	// 渲染/搜索路径。上限 MaxCronTitleLen 字节。
	// 引入背景：docs/rfc/cron-v2-polish.md §3.1。
	Title string `json:"title,omitempty"`

	// Optional working directory override for the CLI process.
	WorkDir string `json:"work_dir,omitempty"`

	// Backend pins the CLI backend (e.g. "claude" / "kiro") this job runs
	// on. Empty = router default — old cron_jobs.json without this field
	// deserialise to "" and continue routing through the operator's
	// configured default, so there is zero migration work for existing
	// installs. The Scheduler propagates this value to AgentOpts.Backend
	// at execute time; validateBackend in the session router still gates
	// shape-invalid input, and wrapperFor falls back to default for
	// unknown but well-formed backend IDs.
	// 引入背景：docs/rfc/multi-backend.md §9 Sprint 6c。
	Backend string `json:"backend,omitempty"`

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

	// LastSessionID 是最近一次成功执行产生的 Claude session_id。持久化后
	// 供 registerStub 注入到新创建的 cron stub 的 prevSessionIDs，让
	// dashboard 点击 cron 侧边栏时 history.Source 能按这个 ID 从
	// ~/.claude/projects 里加载 JSONL 历史。没有它的话 fresh_context=true
	// 场景下每次 Reset 都会清掉 stub 的 chain IDs，stub 的事件面板
	// 就永远是空的。仅 Send 成功路径写入；错误路径保留上一次的值。
	LastSessionID string `json:"last_session_id,omitempty"`

	// LastErrorClass 是 LastError 的机器可读分类（见 ErrorClass 常量）。
	// 与 LastError 同时写入：错误路径 ErrorClass 非空 + ErrorMsg 同步；成功
	// 路径两者均清零。前端用它选图标/着色，不再 substring-grep LastError。
	// 旧 cron_jobs.json 反序列化后为空串，前端 fallback 到 LastError 是否
	// 非空判断"是否失败"——双向兼容。
	// 引入背景：docs/rfc/cron-run-history.md §9。
	LastErrorClass ErrorClass `json:"last_error_class,omitempty"`

	// RunCounters 是每个 job 的累计计数。落盘后 list API 直接读，避免每次
	// 扫描 runs/<jobID>/ 目录。P0 阶段只维护 total/succeeded/failed/skipped/
	// timed_out/canceled；avg_ms / p95_ms 在 P1 引入（EWMA + P²-quantile）。
	// 旧 cron_jobs.json 反序列化为零值，与"从未跑过"不可区分——这是预期：
	// 计数从首次 run 累积，不回填。
	// 引入背景：docs/rfc/cron-run-history.md §3.2。
	RunCounters JobRunCounters `json:"run_counters,omitempty"`

	entryID robfigcron.EntryID // runtime only, not persisted
}

// RunState 是单次 cron 执行的终态分类。运行中态不进 RunState（用 runInflight
// 表达），只有进入持久化路径的 run 才会带 State。
type RunState string

const (
	RunStateRunning   RunState = "running" // 仅 inflight 用，不落盘
	RunStateSucceeded RunState = "succeeded"
	RunStateFailed    RunState = "failed"
	RunStateSkipped   RunState = "skipped"
	RunStateTimedOut  RunState = "timed_out"
	RunStateCanceled  RunState = "canceled"
)

// TriggerKind 标识 run 的触发来源。manual = TriggerNow，scheduled = robfig
// tick，catchup 给未来 missed-schedule 重跑保留位（P3）。
type TriggerKind string

const (
	TriggerScheduled TriggerKind = "scheduled"
	TriggerManual    TriggerKind = "manual"
	// TriggerCatchup is reserved for the missed-schedule replay path (P3,
	// not yet implemented). No production code emits it today; consumers
	// should treat unknown trigger strings as forward-compatible.
	TriggerCatchup TriggerKind = "catchup"
)

// ErrorClass 是 cron run 错误的机器可读分类。executeOpt 各失败分支映射到
// 固定常量，UI/metrics 据此分组，不再字符串匹配 LastError。
//
// 设计取舍：state 只表达终态（succeeded/failed/skipped/timed_out/canceled），
// ErrorClass 表达"为什么 not succeeded"。例如 timed_out 都是 deadline_exceeded，
// canceled 都是 context.Canceled——两者强相关，但分开存便于将来加新 class
// 不动 state 枚举。
type ErrorClass string

const (
	ErrClassNone               ErrorClass = ""
	ErrClassSessionError       ErrorClass = "session_error"
	ErrClassSendError          ErrorClass = "send_error"
	ErrClassDeadlineExceeded   ErrorClass = "deadline_exceeded"
	ErrClassCanceled           ErrorClass = "canceled"
	ErrClassWorkDirUnreachable ErrorClass = "workdir_unreachable"
	ErrClassWorkDirOutsideRoot ErrorClass = "workdir_outside_root"
	ErrClassOverlapSkipped     ErrorClass = "overlap_skipped"
	ErrClassPausedConcurrent   ErrorClass = "paused_concurrent"
	// ErrClassPanic is reserved for the future panic-recovery path
	// (P3, not yet implemented); finishRun does not emit it today.
	ErrClassPanic ErrorClass = "panic"
)

// hexIDBytes 是所有 cron 内部 ID（jobID / runID）的熵字节数。固定 8 字节
// = 16 hex 字符；想加宽时改这一个常量即可两侧同步，避免 ID 宽度漂移。
// R220-GO-2: 之前 generateID 与 generateRunID 各自定义 8，注释口口声声
// "为将来扩展分离"但其实就是同源逻辑——把字节数提到常量后再保留两个
// 公开名字以维持调用语义。
const hexIDBytes = 8

// generateHexID 返回 hexIDBytes 个 crypto/rand 字节的小写 hex 表示。
// crypto/rand 在 Linux 下来自 getrandom(2)，失败仅在内核 entropy 池不
// 可用的极端场景；视作不可恢复的环境错误，panic 等同于 fatal。
func generateHexID() string {
	b := make([]byte, hexIDBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	// R228-CR-9: hex.EncodeToString skips fmt's reflection path; matches
	// textutil/uuid.go encoding style.
	return hex.EncodeToString(b)
}

// generateRunID 返回 CronRun.RunID（16-char hex）。语义上独立于 jobID，
// 共享 generateHexID 实现。
func generateRunID() string { return generateHexID() }

// generateID 返回 cron Job.ID（16-char hex）。
func generateID() string { return generateHexID() }

// MaxCronTitleLen 是 Job.Title 的字符上限（UTF-8 rune 计）。256 覆盖绝大多数
// 人类可读名称，且与 dashboard 的 escAttr 线长相容。导出以便 server 包
// 在 handler 层复用同一上限，避免两处数字不同步漂移。
const MaxCronTitleLen = 256

// titleFallbackRuneLimit 是 Title 为空时 UI/搜索用 Prompt 首行截断的
// 长度上限（按 rune 算，避免切断中文）。60 rune 与卡片视觉宽度对齐。
const titleFallbackRuneLimit = 60

// jobTitleOrFallback 返回用于 UI 显示 / 搜索主 key 的人类可读名称：
//  1. 如果 Job.Title 非空，直接返回（Trim 后）。
//  2. 否则取 Prompt 的首个非空行，截断到 titleFallbackRuneLimit rune。
//  3. 若 Prompt 也为空，返回空字符串——调用方（UI 层）自行决定占位符。
//
// 包内私有：当前唯一调用者是 cron 包自身（搜索/通知/侧边栏元数据），
// 前端 dashboard 显示走 cron_jobs.json 的 title 字段并独立做 fallback；
// 没有跨包消费者。R232-CR-9 把它从导出降回 unexported。
func jobTitleOrFallback(j *Job) string {
	if j == nil {
		return ""
	}
	if t := strings.TrimSpace(j.Title); t != "" {
		return t
	}
	// R222-CR-5: 抽到 textutil.FirstLine 共用 dispatch/cron 同语义（TrimSpace 后
	// 跨任意数量空白行扫第一非空行），消除三处独立实现的字面 firstLine 漂移风险。
	line := textutil.FirstLine(j.Prompt)
	if line == "" {
		return ""
	}
	// R228-CR-5: 改用 textutil.TruncateRunesNoEllipsis 复用 byte-level 解码 +
	// 短路快路径（len ≤ maxRunes 时无需解码 UTF-8），消除 []rune(line) 的全
	// 量 heap 分配。textutil 的 ASCII "..." 后缀与 cron 卡片的 U+2026 风格
	// 不一致，所以本地补 "…"。靠返回 string 与原值 != 判断是否真发生截断
	// 比再做一次 utf8.RuneCountInString(line) 便宜。
	truncated := textutil.TruncateRunesNoEllipsis(line, titleFallbackRuneLimit)
	if truncated != line {
		return truncated + "…"
	}
	return truncated
}

// cronParser is the shared parser for all schedule validation and preview.
var cronParser = robfigcron.NewParser(
	robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor,
)

// minCronInterval is the minimum allowed interval between cron runs.
// Prevents resource exhaustion from overly frequent schedules like "@every 1s".
const minCronInterval = 5 * time.Minute

// computeJobTimeout returns the per-run deadline for a job. The timeout is
// always maxCap (SchedulerConfig.ExecTimeout) — independent of schedule
// period.
//
// Why no period scaling: a long-running task (e.g. hourly job that takes 70m)
// should not be killed mid-flight just because the next scheduled tick is
// approaching. robfig/cron's SkipIfStillRunning chain wrapper already handles
// that case correctly: the next scheduled tick is dropped, the in-flight run
// continues, and the tick after that gets a clean slot. The schedule parameter
// is kept for signature stability and future extension.
func computeJobTimeout(schedule string, maxCap time.Duration) time.Duration {
	_ = schedule
	return maxCap
}

// schedulePeriod 估算给定 cron 表达式在参考时刻 now 附近的周期（相邻两次
// 触发的间隔）。通过 sched.Next 两次外推实现，精度对 "每 N 分钟 /
// 每天 HH:MM" 这类常见形态足够。无法解析 / 不等间隔（DST 切换窗口）
// 时返回 0，调用方自行决定 fallback。
//
// now 必须由调用方显式提供，保证和上层 HasMissedSchedule /
// previousTickBefore 读取的"现在"完全同步——避免在 DST 切换或 NTP 校
// 正瞬间两者跨越不同小时，导致 period 估成 23h/25h 而产生 missed 假
// 判定。computeJobTimeout / applyJitter 不在意这种纳秒级 skew，传
// time.Now() 即可。
func schedulePeriod(schedule string, now time.Time) time.Duration {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return 0
	}
	first := sched.Next(now)
	second := sched.Next(first)
	return second.Sub(first)
}

// previousTickBefore 算给定 schedule 在 now 之前最近一次应该触发的时刻。
// robfig/cron 只提供 Next()，没有 Prev()。这里用"从 now 回推 3 × period
// 的窗口，在窗口内用 sched.Next(起点) 逼近最接近 now 的 tick"的办法：
//
//  1. 先估计 period（Next 两次）
//  2. 起点 = now - 3 × period（保证至少覆盖一个完整周期）
//  3. 起点不断 Next，直到下一次 Next 超过 now；此时当前 Next 即为"最后
//     一次 ≤ now 的触发时刻"。
//
// 窗口乘 3 是为了应对 DST / 月份 / 闰年这类非等间隔形态（每月 29 日
// 在 2 月可能 "跳 31 天"），给足裕量。每次 Next 是 O(1)，循环最多跑
// 3-5 次，开销可忽略。无法解析的 schedule 返回零值 time。
func previousTickBefore(schedule string, now time.Time) time.Time {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}
	}
	period := schedulePeriod(schedule, now)
	if period <= 0 {
		return time.Time{}
	}
	// 回推起点；加一个安全系数 3 应对月份/DST 的非等距触发
	start := now.Add(-3 * period)
	prev := time.Time{}
	// 上限守卫：极端 DST/月底场景下若 sched.Next 进展极慢，避免
	// 在 dashboard 1Hz 轮询路径短暂阻塞。1000 次足以覆盖任何
	// 合法 cron schedule 在 3×period 窗口内的迭代次数。
	for i := 0; i < 1000; i++ {
		next := sched.Next(start)
		if !next.Before(now) {
			return prev
		}
		prev = next
		start = next
	}
	return prev
}

// HasMissedSchedule 判断 Job 是否曾经错过调度（进程休眠或重启空窗期）。
// 返回 (missed, prevExpectedAt)：prevExpectedAt 是"按 schedule 算上一次
// 应该跑的时刻"，调用方可用来显示 "上次应跑于 …"。
//
// 判定规则：
//  1. schedule 无法解析 / period<=0 → 不算 missed（保守）。
//  2. startedAt 不为零且 now - startedAt < 5 × period：刚启动的抑制窗口，
//     避免刚 boot 时所有长周期 job 都被误判 missed。测试可以传
//     time.Time{} 绕过。
//  3. 从未跑过 (LastRunAt.IsZero)：若 now - CreatedAt > period 则判 missed
//     （任务创建后本应至少跑过一次）。
//  4. 跑过：若 prevExpectedAt - LastRunAt > period × 1.5 则判 missed
//     （允许 50% 裕量应对 jitter + 轻微延迟）。
//
// 关联：docs/rfc/cron-v2-polish.md §3.3 Increment C。
func HasMissedSchedule(j *Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	period := schedulePeriod(j.Schedule, now)
	if period <= 0 {
		return false, time.Time{}
	}
	// 启动抑制：刚 boot 时所有 long-period job 都会"错过"，这是可预期的。
	// 5 × period 给足让第一轮调度落地的余量。
	if !startedAt.IsZero() && now.Sub(startedAt) < 5*period {
		return false, time.Time{}
	}
	prev := previousTickBefore(j.Schedule, now)
	if prev.IsZero() {
		return false, time.Time{}
	}
	if j.LastRunAt.IsZero() {
		// 从未跑过：看任务本身存在了多久
		if !j.CreatedAt.IsZero() && now.Sub(j.CreatedAt) > period {
			return true, prev
		}
		return false, time.Time{}
	}
	// 跑过：对比上次跑的时刻和"上次应跑的时刻"
	if prev.Sub(j.LastRunAt) > period*3/2 {
		return true, prev
	}
	return false, time.Time{}
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
