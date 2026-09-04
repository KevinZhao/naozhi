package cron

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/textutil"
)

// JobIMContext bundles the originating IM-channel coordinates a cron Job
// inherits from the message that created it. Kept as a tiny local type (not
// platform.IncomingMessage) so external callers don't import the platform
// package to construct a Job. All fields are optional: dashboard-created jobs
// leave it zero-value.
type JobIMContext struct {
	Platform  string
	ChatID    string
	ChatType  string
	CreatedBy string
}

// JobInit bundles every operator-settable field a cron Job can carry at
// creation time — the input to NewJobFull, covering the dashboard-only fields
// (Title / WorkDir / Notify* / FreshContext / Backend / Paused) that NewJob's
// (schedule, prompt, JobIMContext) signature cannot express (#1142).
// All fields are optional; the zero value yields a Job equivalent to
// NewJob(schedule, prompt, ctx) with empty inputs.
type JobInit struct {
	Schedule string
	Prompt   string
	IM       JobIMContext

	Title          string
	WorkDir        string
	Backend        string
	Placement      string
	NotifyPlatform string
	NotifyChatID   string
	Notify         *bool
	SideEffects    *bool
	FreshContext   bool
	Paused         bool
}

// NewJob constructs a Job ready to hand to Scheduler.AddJob from the
// (schedule, prompt) pair plus the IM-channel context that originated it.
// The dashboard path that also needs Title / WorkDir / Notify* / FreshContext
// / Backend / Paused must use NewJobFull so no surface hand-rolls a cron.Job{}
// literal (#1142).
//
// CreatedAt is intentionally NOT stamped here: AddJob owns Job persistence
// and needs a single coherent timestamp source.
func NewJob(schedule, prompt string, ctx JobIMContext) *Job {
	return NewJobFull(JobInit{Schedule: schedule, Prompt: prompt, IM: ctx})
}

// NewJobFull constructs a Job from the full JobInit field set so every
// surface routes through one field-mapping site (#1142). CreatedAt is left
// zero for AddJob to stamp, mirroring NewJob.
func NewJobFull(in JobInit) *Job {
	return &Job{
		Schedule:       in.Schedule,
		Prompt:         in.Prompt,
		Platform:       in.IM.Platform,
		ChatID:         in.IM.ChatID,
		ChatType:       in.IM.ChatType,
		CreatedBy:      in.IM.CreatedBy,
		Title:          in.Title,
		WorkDir:        in.WorkDir,
		Backend:        in.Backend,
		Placement:      in.Placement,
		NotifyPlatform: in.NotifyPlatform,
		NotifyChatID:   in.NotifyChatID,
		Notify:         in.Notify,
		SideEffects:    in.SideEffects,
		FreshContext:   in.FreshContext,
		Paused:         in.Paused,
	}
}

// cronEntryID is the cron-local alias for robfig/cron's per-entry handle so
// a future cron-engine swap touches one declaration (#977).
type cronEntryID = robfigcron.EntryID

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
	// 为空时 UI 回退到 Prompt 首行（见 jobTitleOrFallback），兼容旧
	// cron_jobs.json。上限 MaxCronTitleLen 字节。
	Title string `json:"title,omitempty"`

	// Optional working directory override for the CLI process.
	WorkDir string `json:"work_dir,omitempty"`

	// Backend pins the CLI backend (e.g. "claude" / "kiro") this job runs
	// Backend pins the CLI backend (e.g. "claude" / "kiro") this job runs on.
	// Empty = router default, so old cron_jobs.json needs no migration. Propagated
	// to AgentOpts.Backend at execute time; validateBackend still gates
	// shape-invalid input and wrapperFor falls back for unknown IDs.
	Backend string `json:"backend,omitempty"`

	// Placement selects WHERE the job runs: "" / "local" = this host via the
	// session router; "sandbox" = run-once AgentCore microVM. validatePlacement
	// gates every write path; executeOpt branches on it before touching the
	// router. Sandbox guardrails are enforced by the sandbox executor, not here.
	Placement string `json:"placement,omitempty"`

	// SideEffects declares that this job mutates external state (pushes a
	// branch, opens a PR…). Tri-state like Notify: nil = legacy default (false).
	// A sandbox run ending failed-transport (microVM fate unknown) goes to the
	// human confirmation queue instead of auto-replay when this is true.
	SideEffects *bool `json:"side_effects,omitempty"`

	// Optional notification target for dashboard-created jobs.
	// When set, execution results are also sent to this IM channel.
	NotifyPlatform string `json:"notify_platform,omitempty"`
	NotifyChatID   string `json:"notify_chat_id,omitempty"`

	// Notify controls whether execution results are pushed to an IM channel
	// after each run. Tri-state pointer so old jobs (nil) keep legacy behaviour:
	// IM-created jobs reply to their source chat; dashboard-created jobs honor
	// per-job NotifyPlatform/NotifyChatID if set. Explicit true/false toggles
	// delivery to the scheduler's notify_default target (or per-job override).
	Notify *bool `json:"notify,omitempty"`

	// FreshContext, when true, resets the cron session before each run so the
	// CLI starts from a clean slate; false keeps the session long-lived and each
	// run appends a turn to the accumulated context.
	FreshContext bool `json:"fresh_context,omitempty"`

	// Last execution result, persisted across restarts. LastRunAt has no
	// omitempty: encoding/json never drops a zero time.Time; callers check IsZero.
	LastResult string    `json:"last_result,omitempty"`
	LastRunAt  time.Time `json:"last_run_at"`
	LastError  string    `json:"last_error,omitempty"`

	// LastSessionID 是最近一次成功执行产生的 Claude session_id。registerStub
	// 把它注入 cron stub 的 prevSessionIDs，让 dashboard 侧边栏能按此 ID 加载
	// JSONL 历史；否则 fresh_context=true 下每次 Reset 后 stub 事件面板永远为空。
	// 仅 Send 成功路径写入；错误路径保留上一次的值。
	LastSessionID string `json:"last_session_id,omitempty"`

	// LastErrorClass 是 LastError 的机器可读分类（见 ErrorClass 常量），与
	// LastError 同时写入/清零。旧 cron_jobs.json 反序列化后为空串，前端
	// fallback 到 LastError 是否非空——双向兼容。
	LastErrorClass ErrorClass `json:"last_error_class,omitempty"`

	// RunCounters 是每个 job 的累计计数，落盘后 list API 直接读，避免扫描
	// runs/<jobID>/。旧 cron_jobs.json 反序列化为零值，与"从未跑过"不可区分
	// ——计数从首次 run 累积，不回填。
	RunCounters JobRunCounters `json:"run_counters,omitempty"`

	entryID cronEntryID // runtime only, not persisted

	// cachedPeriod is the schedule period (Next-Next delta) precomputed by
	// registerJob so the hot jitter path skips 2× sched.Next per tick. Zero =
	// not yet registered; callers fall back to the live computation (#664).
	cachedPeriod time.Duration // runtime only, not persisted

	// cachedSched is the parsed robfigcron.Schedule populated by registerJob so
	// HasMissedScheduleCached skips cronParser.Parse on every 1Hz tick (#477).
	// nil = not yet registered; callers fall back to the parse path.
	cachedSched robfigcron.Schedule // runtime only, not persisted
}

// RunState 是单次 cron 执行的终态分类。运行中态不进 RunState（用 runInflight
// 表达）。它是 runtelemetry.RunState 的 type alias，与 sysession 共享同一
// wire vocabulary；新增 RunState 加在 runtelemetry/state.go 单一来源处 (#1317)。
type RunState = runtelemetry.RunState

const (
	RunStateSucceeded = runtelemetry.RunStateSucceeded
	RunStateFailed    = runtelemetry.RunStateFailed
	RunStateSkipped   = runtelemetry.RunStateSkipped
	RunStateTimedOut  = runtelemetry.RunStateTimedOut
	RunStateCanceled  = runtelemetry.RunStateCanceled
)

// TriggerKind 标识 run 的触发来源。manual = TriggerNow，scheduled = robfig
// tick，catchup 给未来 missed-schedule 重跑保留位。runtelemetry.TriggerKind
// 的 type alias；新 trigger value 加在 runtelemetry 单一来源处 (#1317)。
type TriggerKind = runtelemetry.TriggerKind

const (
	TriggerScheduled = runtelemetry.TriggerScheduled
	TriggerManual    = runtelemetry.TriggerManual
	// TriggerCatchup is reserved for the missed-schedule replay path (not yet
	// implemented). No production code may emit it until that design is settled;
	// consumers treat unknown trigger strings as forward-compatible.
	TriggerCatchup = runtelemetry.TriggerCatchup
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
	// ErrClassRouterMissing fires when executeOpt short-circuits on a nil router
	// (test fixtures or a misconfigured scheduler); a started→ended pair is still
	// emitted so dashboard "running" counters stay consistent (#1323).
	ErrClassRouterMissing ErrorClass = "router_missing"
	// ErrClassPausedConcurrent fires when the post-CAS recheck sees the job
	// switched to Paused between the dispatch lookup and the inflight CAS; a
	// synthetic started→ended pair keeps subscriber timelines gap-free (#1410).
	ErrClassPausedConcurrent ErrorClass = "paused_concurrent"
	// ErrClassDeletedConcurrent fires when the post-CAS recheck sees the job
	// removed from s.jobs in the same cross-lock window (#1410).
	ErrClassDeletedConcurrent ErrorClass = "deleted_concurrent"
	// ErrClassPanic is reserved for the future panic-recovery path
	// (P3, not yet implemented); finishRun does not emit it today.
	ErrClassPanic ErrorClass = "panic"
	// Sandbox placement classes; wire values mirror runtelemetry.ErrClassCronSandbox*.
	// Transport is the double-run-risk state (microVM fate unknown).
	ErrClassSandboxFailed      ErrorClass = "sandbox_failed"
	ErrClassSandboxTransport   ErrorClass = "sandbox_transport"
	ErrClassSandboxUnavailable ErrorClass = "sandbox_unavailable"
)

// hexIDEntropyBytes 是所有 cron 内部 ID（jobID / runID）的熵字节数（不是
// 字符数）：8 字节 → 16 hex 字符。想加宽时改这一个常量即可两侧同步。
const hexIDEntropyBytes = 8

// generateHexID 返回 hexIDEntropyBytes 个 crypto/rand 字节的小写 hex 表示。
// 失败返回 error 而非 panic：AddJob 把错误透传给 HTTP / IM caller，周期
// tick 路径 log + 跳过该次执行，进程存活、下一 tick 自然恢复 (#706)。
//
// 用 io.ReadFull(rand.Reader, b) 而非 rand.Read：Go 1.26 起 rand.Read 在
// reader 失败时直接 runtime fatal（go.dev/issue/66821），调用方拿不到 error。
// 其它子系统生成 hex ID 时应以本函数为 shape 参考 (#1313)。
// 测试需要 panic 语义时用 mustGenerateHexID，别在生产路径 catch 后 panic。
func generateHexID() (string, error) {
	// Fixed-size stack array: make([]byte, n) escaped to the heap because
	// io.ReadFull takes an interface the compiler cannot prove non-retaining.
	var b [hexIDEntropyBytes]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", fmt.Errorf("cron: crypto/rand unavailable: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// generateRunID 返回 CronRun.RunID（16-char hex）。语义上独立于 jobID，
// 共享 generateHexID 实现。
func generateRunID() (string, error) { return generateHexID() }

// generateID 返回 cron Job.ID（16-char hex）。
func generateID() (string, error) { return generateHexID() }

// IsValidID reports whether s is a valid cron / cron-run identifier: a
// non-empty lowercase hex string of at most 64 bytes. Job and run IDs are
// 16 hex chars today; the 64-byte bound is reserved for a schema bump.
// Uppercase hex, path characters and temp/backup suffixes are all rejected,
// so store entry points (parse / list / append / detail handler) can filter
// stray files under runs/<jobID>/ and HTTP handlers can reject bad IDs
// before any disk IO. Lives in job.go as the ID-schema home (#990).
func IsValidID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// MaxCronTitleLen 是 Job.Title 的字符上限（UTF-8 rune 计）。256 覆盖绝大多数
// 人类可读名称，且与 dashboard 的 escAttr 线长相容。导出以便 server 包
// 在 handler 层复用同一上限，避免两处数字不同步漂移。
const MaxCronTitleLen = 256

// JobRunCounters is the per-Job cumulative counter Job persists alongside
// LastRunAt / LastResult / LastError. Maintained on every finishRun so the
// dashboard list endpoint shows terminal-state tallies without rescanning
// runs/<jobID>/.
type JobRunCounters struct {
	Total     int64 `json:"total,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Skipped   int64 `json:"skipped,omitempty"`
	TimedOut  int64 `json:"timed_out,omitempty"`
	Canceled  int64 `json:"canceled,omitempty"`
}

// addRun 把一次终态 run 累加到 counters。调用方持 s.mu.Lock。
func (c *JobRunCounters) addRun(state RunState) {
	c.Total++
	switch state {
	case RunStateSucceeded:
		c.Succeeded++
	case RunStateFailed:
		c.Failed++
	case RunStateSkipped:
		c.Skipped++
	case RunStateTimedOut:
		c.TimedOut++
	case RunStateCanceled:
		c.Canceled++
	}
}

// titleFallbackRuneLimit 是 Title 为空时 UI/搜索用 Prompt 首行截断的
// 长度上限（按 rune 算，避免切断中文）。60 rune 与卡片视觉宽度对齐。
const titleFallbackRuneLimit = 60

// jobTitleOrFallback 返回用于 UI 显示 / 搜索主 key 的人类可读名称：
//  1. 如果 Job.Title 非空，直接返回（Trim 后）。
//  2. 否则取 Prompt 的首个非空行，截断到 titleFallbackRuneLimit rune。
//  3. 若 Prompt 也为空，返回空字符串——调用方自行决定占位符。
func jobTitleOrFallback(j *Job) string {
	if j == nil {
		return ""
	}
	if t := strings.TrimSpace(j.Title); t != "" {
		return t
	}
	// textutil.FirstLine 与 dispatch 同语义（跨任意空白行取第一非空行）。
	line := textutil.FirstLine(j.Prompt)
	if line == "" {
		return ""
	}
	// TruncateRunesNoEllipsis 走 byte-level 快路径且不分配 []rune；textutil 的
	// ASCII "..." 与卡片的 U+2026 风格不一致，所以本地补 "…"。
	truncated := textutil.TruncateRunesNoEllipsis(line, titleFallbackRuneLimit)
	if truncated != line {
		return truncated + "…"
	}
	return truncated
}

// cronParseOptions is the single source of truth for the field set the cron
// schedule grammar accepts: standard 5-field (Minute/Hour/Dom/Month/Dow) plus
// @descriptors (@daily, @every 5m, …). Widening (e.g. Second) changes exactly
// this constant (#988).
const cronParseOptions = robfigcron.Minute | robfigcron.Hour | robfigcron.Dom |
	robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor

// cronParser is the shared parser for all schedule validation and preview.
var cronParser = robfigcron.NewParser(cronParseOptions)

// minCronInterval is the minimum allowed interval between cron runs.
// Prevents resource exhaustion from overly frequent schedules like "@every 1s".
const minCronInterval = 5 * time.Minute

// missed-schedule heuristics for HasMissedSchedule.
//
// missedScheduleSuppressFactor: boot grace, suppress "missed" verdicts during
// the first N×period after process start so long-period jobs don't always
// look behind on the first dashboard read.
//
// missedScheduleSlack{Num,Den}: tolerate prev-tick vs LastRunAt drift up to
// Num/Den × period before declaring a miss (1.5× by default; relaxes the
// bound for jobs that ran slightly late).
const (
	missedScheduleSuppressFactor = 5
	missedScheduleSlackNum       = 3
	missedScheduleSlackDen       = 2
)

// schedulePeriod 估算给定 cron 表达式在 now 附近的周期（sched.Next 两次外推）。
// 无法解析 / 不等间隔（DST 切换窗口）时返回 0。now 必须由调用方显式提供，
// 与 HasMissedSchedule / previousTickBefore 读取的"现在"完全同步，避免跨
// 越 DST/NTP 校正瞬间把 period 估成 23h/25h 而误判 missed。
func schedulePeriod(schedule string, now time.Time) time.Duration {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return 0
	}
	return schedulePeriodFromSched(sched, now)
}

// previousTickBefore 算给定 schedule 在 now 之前最近一次应该触发的时刻。
// robfig/cron 只有 Next()：从 now - 3×period 起反复 Next 直到超过 now，
// 上一个值即为答案。窗口乘 3 是给 DST / 月份 / 闰年这类非等间隔形态留裕量。
// 无法解析的 schedule 返回零值 time。生产路径走 previousTickBeforeFromSched
// 复用已 Parse 的 sched；本字符串入口仅供包内测试直测回推逻辑。
func previousTickBefore(schedule string, now time.Time) time.Time {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}
	}
	period := schedulePeriodFromSched(sched, now)
	if period <= 0 {
		return time.Time{}
	}
	return previousTickBeforeFromSched(sched, period, now)
}

// schedulePeriodFromSched 同 schedulePeriod，但接受已解析的 robfigcron.Schedule，
// 避免在 HasMissedSchedule 路径上重复 Parse。
func schedulePeriodFromSched(sched robfigcron.Schedule, now time.Time) time.Duration {
	first := sched.Next(now)
	second := sched.Next(first)
	return second.Sub(first)
}

// previousTickBeforeFromSched 同 previousTickBefore，但接受已解析的 sched +
// 已知 period。previousTickMaxIter 足以覆盖任何合法 schedule 在 3×period
// 窗口内的迭代次数。
func previousTickBeforeFromSched(sched robfigcron.Schedule, period time.Duration, now time.Time) time.Time {
	if period <= 0 {
		return time.Time{}
	}
	start := now.Add(-3 * period)
	prev := time.Time{}
	for i := 0; i < previousTickMaxIter; i++ {
		next := sched.Next(start)
		if !next.Before(now) {
			return prev
		}
		prev = next
		start = next
	}
	return prev
}

// HasMissedSchedule 判断 Job 是否曾经错过调度（进程休眠或重启空窗期），
// 返回 (missed, prevExpectedAt)。规则：
//  1. schedule 无法解析 / period<=0 → 不算 missed（保守）。
//  2. now - startedAt < missedScheduleSuppressFactor × period：刚启动抑制窗口；
//     测试可传 time.Time{} 绕过。
//  3. 从未跑过：now - CreatedAt > period × 1.5 则判 missed（paused 除外）。
//  4. 跑过：prevExpectedAt - LastRunAt > period × 1.5 则判 missed（裕量应对
//     jitter + 轻微延迟）。
func HasMissedSchedule(j *Job, now, startedAt time.Time) (bool, time.Time) {
	return hasMissedScheduleImpl(j, nil, 0, now, startedAt)
}

// HasMissedScheduleCached is the alloc-free variant of HasMissedSchedule for
// the dashboard 1Hz handleList fanout (#477): when registerJob has run,
// j.cachedSched / j.cachedPeriod skip the cronParser.Parse regex. Falls back
// to the parse path on a cold cache so behaviour matches HasMissedSchedule.
func HasMissedScheduleCached(j *Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	return hasMissedScheduleImpl(j, j.cachedSched, j.cachedPeriod, now, startedAt)
}

// hasMissedScheduleImpl is the shared body of HasMissedSchedule and
// HasMissedScheduleCached. cached (non-nil) skips the regex parse and
// cachedPeriod (>0) skips the 2× sched.Next; zero values recompute.
func hasMissedScheduleImpl(j *Job, cached robfigcron.Schedule, cachedPeriod time.Duration, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	sched := cached
	if sched == nil {
		var err error
		sched, err = cronParser.Parse(j.Schedule)
		if err != nil {
			return false, time.Time{}
		}
	}
	period := cachedPeriod
	if period <= 0 {
		period = schedulePeriodFromSched(sched, now)
	}
	if period <= 0 {
		return false, time.Time{}
	}
	if !startedAt.IsZero() && now.Sub(startedAt) < missedScheduleSuppressFactor*period {
		return false, time.Time{}
	}
	prev := previousTickBeforeFromSched(sched, period, now)
	if prev.IsZero() {
		return false, time.Time{}
	}
	if j.LastRunAt.IsZero() {
		// A paused never-run job has had no fair chance to fire; the startedAt
		// suppression only covers process startup, not a job created paused and
		// just resumed (#1979).
		if j.Paused {
			return false, time.Time{}
		}
		// Never-run jobs use the same slack factor as the already-run branch:
		// a bare `> period` threshold flagged a healthy job whose first tick landed
		// inside its jitter window as missed.
		if !j.CreatedAt.IsZero() && now.Sub(j.CreatedAt) > period*missedScheduleSlackNum/missedScheduleSlackDen {
			return true, prev
		}
		return false, time.Time{}
	}
	if prev.Sub(j.LastRunAt) > period*missedScheduleSlackNum/missedScheduleSlackDen {
		return true, prev
	}
	return false, time.Time{}
}

// validateSchedule checks if the cron expression is valid and respects the
// minimum interval. loc is the timezone the scheduler will evaluate the
// schedule in (WithLocation); seeding the probe in the same frame keeps DST /
// month-end forms from passing here but firing faster at runtime (#1321).
// nil falls back to time.Local for callers without a location yet.
func validateSchedule(schedule string, loc *time.Location) error {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return err
	}
	if loc == nil {
		loc = time.Local
	}
	// Probe from a FIXED DST-quiet instant (mid-January noon in loc) rather than
	// time.Now(): a probe straddling a DST transition made a genuine 5-minute
	// schedule look ~1h apart, or an hourly one inflated, depending on when the
	// operator saved the job (#965).
	ref := time.Date(2024, time.January, 15, 12, 0, 0, 0, loc)
	first := sched.Next(ref)
	second := sched.Next(first)
	// No `interval > 0` guard: minCronInterval is positive, so this also rejects
	// zero / negative intervals.
	if interval := second.Sub(first); interval < minCronInterval {
		return fmt.Errorf("interval %v is too short, minimum is %v", interval, minCronInterval)
	}
	return nil
}
