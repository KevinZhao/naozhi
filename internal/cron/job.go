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

// JobInit bundles every operator-settable field a cron Job can carry at
// creation time. It is the fuller input to NewJobFull, covering the
// dashboard-only fields (Title / WorkDir / Notify* / FreshContext / Backend
// / Paused) that the (schedule, prompt, JobIMContext) NewJob signature
// cannot express.
//
// R250-CR-9 (#1142): NewJob's godoc historically claimed it was the single
// construction choke point that protected every cross-package caller from a
// cron.Job{} field rename. That invariant was not actually held — the
// dashboard create handler bypassed NewJob and spelled out a multi-field
// cron.Job{} literal directly because NewJob accepted none of the
// dashboard-specific fields. JobInit + NewJobFull restore the invariant:
// callers needing the richer field set have a constructor to route through
// instead of an open-coded literal. Schedule/Prompt + the embedded
// JobIMContext mirror NewJob so the two constructors stay in lockstep.
//
// All fields are optional; the zero value yields a Job equivalent to
// NewJob(schedule, prompt, ctx) with empty schedule/prompt/context.
type JobInit struct {
	Schedule string
	Prompt   string
	IM       JobIMContext

	Title          string
	WorkDir        string
	Backend        string
	NotifyPlatform string
	NotifyChatID   string
	Notify         *bool
	FreshContext   bool
	Paused         bool
}

// NewJob constructs a Job ready to hand to Scheduler.AddJob from the
// (schedule, prompt) pair plus the IM-channel context that originated it.
// It is the narrow constructor for the dispatch / IM command path, which
// only ever sets those fields; the dashboard path that also needs
// Title / WorkDir / Notify* / FreshContext / Backend / Paused must use
// NewJobFull so neither surface hand-rolls a cron.Job{} literal that a
// field rename could silently break (R250-CR-9 / #1142).
//
// CreatedAt is intentionally NOT stamped here: AddJob is the choke point
// that owns Job persistence and needs a single coherent timestamp source.
// Pre-stamping in NewJob would cause a tiny but real skew (LastRunAt
// comparisons, missed-schedule detection) when the constructor is called
// far ahead of AddJob.
func NewJob(schedule, prompt string, ctx JobIMContext) *Job {
	return NewJobFull(JobInit{Schedule: schedule, Prompt: prompt, IM: ctx})
}

// NewJobFull constructs a Job from the full JobInit field set so the
// dashboard create handler (and any future surface needing the richer
// fields) routes through a constructor instead of an open-coded
// cron.Job{} literal. NewJob delegates here so both constructors share a
// single field-mapping site — a Job field rename now lands in exactly one
// place. CreatedAt is left zero for AddJob to stamp, mirroring NewJob.
// R250-CR-9 (#1142).
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
		NotifyPlatform: in.NotifyPlatform,
		NotifyChatID:   in.NotifyChatID,
		Notify:         in.Notify,
		FreshContext:   in.FreshContext,
		Paused:         in.Paused,
	}
}

// cronEntryID is the cron-local name for robfig/cron's per-entry handle.
// R249-ARCH-11 (#977): the third-party robfigcron.EntryID type name was
// scattered across the Job field, the two list-snapshot pools, every
// pause/resume/update rollback-snapshot local, and cronEntryGoneLocked.
// Aliasing it to one declaration localises the robfig binding so a future
// cron-engine swap (or wrapping the handle in a richer entry struct) touches
// this single line instead of ~15 references. As a type alias it is
// identical to robfigcron.EntryID, so the pool type-assertions and the
// s.cron.Remove/Entry calls keep compiling unchanged.
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

	entryID cronEntryID // runtime only, not persisted

	// cachedPeriod is the precomputed schedule period (Next-Next delta), populated
	// once per registerJob alongside entryID. The hot jitter path (#664 / R242-PERF-2)
	// previously called schedulePeriodFromSched(sched, time.Now()) on every cron tick,
	// running 2× sched.Next per jittered job per fire. Period only changes when
	// Schedule mutates (UpdateJob path re-registers and recomputes); cache it on
	// the Job to skip the per-tick recompute. Zero = unknown / not yet registered;
	// callers fall back to the live computation in that case so test fixtures
	// (which never call registerJob) keep working.
	cachedPeriod time.Duration // runtime only, not persisted

	// cachedSched is the parsed robfigcron.Schedule, populated alongside
	// cachedPeriod by registerJob. Lets HasMissedScheduleCached (the cron-
	// pkg helper exposed for dashboard handleList — R241-PERF-3 / #477)
	// skip the cronParser.Parse regex on every 1Hz tick. nil = unknown /
	// not yet registered; HasMissedScheduleCached falls back to the
	// classic HasMissedSchedule path which re-parses on every call.
	// Test fixtures that build Job by hand (without registerJob) keep
	// working through that fallback.
	cachedSched robfigcron.Schedule // runtime only, not persisted
}

// RunState 是单次 cron 执行的终态分类。运行中态不进 RunState（用 runInflight
// 表达），只有进入持久化路径的 run 才会带 State。
//
// R20260527122801-ARCH-2 (#1317): 该类型现在是
// runtelemetry.RunState 的 type alias，与 sysession 共享同一个 wire
// vocabulary。emitRunStarted/emitRunEnded 内部从 cron.RunState 转
// runtelemetry.RunState 的 cast 退化成 no-op；测试 / 调用方代码
// 不需要改。新增 RunState 应加在 runtelemetry/state.go 单一来源处，
// 这里通过 alias 自动继承。
type RunState = runtelemetry.RunState

const (
	RunStateSucceeded = runtelemetry.RunStateSucceeded
	RunStateFailed    = runtelemetry.RunStateFailed
	RunStateSkipped   = runtelemetry.RunStateSkipped
	RunStateTimedOut  = runtelemetry.RunStateTimedOut
	RunStateCanceled  = runtelemetry.RunStateCanceled
)

// TriggerKind 标识 run 的触发来源。manual = TriggerNow，scheduled = robfig
// tick，catchup 给未来 missed-schedule 重跑保留位（P3）。
//
// R20260527122801-ARCH-2 (#1317): 同 RunState 一样，本类型是
// runtelemetry.TriggerKind 的 type alias。emit* 路径里的 cast 退
// 化成 no-op；新 trigger value 加在 runtelemetry 单一来源处。
type TriggerKind = runtelemetry.TriggerKind

const (
	TriggerScheduled = runtelemetry.TriggerScheduled
	TriggerManual    = runtelemetry.TriggerManual
	// TriggerCatchup is reserved for the missed-schedule replay path (P3,
	// not yet implemented). No production code emits it today; consumers
	// should treat unknown trigger strings as forward-compatible.
	// R235-CR-13: do NOT reference this value from production code paths
	// until the missed-schedule replay design is settled — adding stray
	// emit sites now would freeze a wire format that may still change.
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
	// ErrClassRouterMissing fires when executeOpt's hot-path self-defence
	// short-circuits on a nil router (test fixtures or a misconfigured
	// scheduler). Subscribers see a started→ended pair so dashboard
	// "running" counters stay consistent. R20260527122801-CR-13 (#1323).
	ErrClassRouterMissing ErrorClass = "router_missing"
	// ErrClassPausedConcurrent fires when the post-CAS recheck sees the
	// job switched to Paused between the dispatch lookup and the inflight
	// CAS. R040034-CR-1 (#1410): previously the recheck silently dropped
	// the run with only a Debug log, leaving subscriber timelines with a
	// gap in the 1-2µs cross-lock window. Now mirrors the router-missing
	// precedent and emits a synthetic started→ended pair so dashboards
	// see consistent lifecycle frames.
	ErrClassPausedConcurrent ErrorClass = "paused_concurrent"
	// ErrClassDeletedConcurrent fires when the post-CAS recheck sees the
	// job removed from s.jobs between the dispatch lookup and the
	// inflight CAS. R040034-CR-1 (#1410): paired with PausedConcurrent so
	// the two cross-lock-window outcomes are distinguishable on the wire.
	ErrClassDeletedConcurrent ErrorClass = "deleted_concurrent"
	// ErrClassPanic is reserved for the future panic-recovery path
	// (P3, not yet implemented); finishRun does not emit it today.
	ErrClassPanic ErrorClass = "panic"
)

// hexIDEntropyBytes 是所有 cron 内部 ID（jobID / runID）的熵字节数（不是
// 字符数）。固定 8 字节 → hex.EncodeToString → 16 hex 字符。想加宽时
// 改这一个常量即可两侧同步，避免 ID 宽度漂移。
//
// R247-CR-17: 旧名 hexIDBytes 在 godoc 旁写"16-char hex"造成歧义——
// "Bytes" 究竟是熵字节还是输出字符？改名 hexIDEntropyBytes 把语义钉死
// 在熵源侧，调用点 make([]byte, hexIDEntropyBytes) 一眼可读。
// R220-GO-2: 之前 generateID 与 generateRunID 各自定义 8，注释口口声声
// "为将来扩展分离"但其实就是同源逻辑——把字节数提到常量后再保留两个
// 公开名字以维持调用语义。
const hexIDEntropyBytes = 8

// generateHexID 返回 hexIDEntropyBytes 个 crypto/rand 字节的小写 hex 表示。
// crypto/rand 在 Linux 下来自 getrandom(2)，失败仅在内核 entropy 池不
// 可用的极端场景；返回 error 让 caller 自行选择降级方式。
//
// R242-CR-14 (#706): 历史实现 panic("crypto/rand unavailable: ...")，会从
// AddJob 的 caller stack 一路炸到 dashboard handler / IM 入口；进程级
// crash 远比"这一次 add/run 失败"破坏性大。改返 error 后：
//   - AddJob 把错误透传给 HTTP / IM caller，请求层失败可重试；
//   - 周期 tick 路径（executeOpt / emitOverlapSkipped）log + 跳过该次
//     执行，保留进程存活、下一 tick 自然恢复。
//
// 实现细节：Go 1.26 起 `crypto/rand.Read` 在 reader 失败时直接调
// runtime fatal（go.dev/issue/66821），调用方拿不到 error。所以这里
// 改用 io.ReadFull(rand.Reader, b) 直接读底层 Reader —— 如此 Read
// 的 err 才会原样返回给我们，给 fault-injection 测试以及未来生产环境
// 真正的 entropy 不足故障都留下处理路径。
//
// CANONICAL-HEX-ID-PATTERN (R20260527122801-ARCH-7 / #1313): cron 是首个
// 切到 io.ReadFull(rand.Reader, b) 的子系统。其它生成 hex ID 的
// 子系统（internal/sysession/run.go、internal/server/dashboard_send.go、
// internal/session/scratch.go 等）目前仍调用旧 rand.Read，Go 1.26 entropy
// 不足时会 fatal 整个进程。下次共享重构应抽 osutil.GenerateHexID +
// MustGenerateHexID 把所有点迁移到这个 pattern；在那之前，本函数是
// shape 参考，修改时（输出宽度 / 错误格式）需通知其它子系统同步。
//
// 测试需要 panic 等价语义时用 mustGenerateHexID（test helper），别在
// 生产路径 catch error 后 panic —— 那等于把这次重构反向回去。
func generateHexID() (string, error) {
	b := make([]byte, hexIDEntropyBytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("cron: crypto/rand unavailable: %w", err)
	}
	// R228-CR-9: hex.EncodeToString skips fmt's reflection path; matches
	// textutil/uuid.go encoding style.
	return hex.EncodeToString(b), nil
}

// generateRunID 返回 CronRun.RunID（16-char hex）。语义上独立于 jobID，
// 共享 generateHexID 实现。
func generateRunID() (string, error) { return generateHexID() }

// generateID 返回 cron Job.ID（16-char hex）。
func generateID() (string, error) { return generateHexID() }

// IsValidID reports whether s is a valid cron / cron-run identifier:
// a non-empty lowercase hex string of at most 64 bytes. Currently job
// and run IDs are generated as 16 hex chars; the 64-byte upper bound
// is held in reserve for a future schema bump.
//
// Accepts (returns true):
//   - "0123456789abcdef"               — canonical 16-char job/run ID
//   - "abc123"                         — short lowercase hex
//   - strings.Repeat("a", 64)          — at the 64-byte boundary
//
// Rejects (returns false):
//   - ""                               — empty
//   - "ABC123"                         — uppercase hex (lowercase only)
//   - "abc-123" / "abc.tmp" / "abc~"   — non-hex chars (rejects temp
//     files, backups, .DS_Store, etc. that may appear in runs/<jobID>/)
//   - "../etc/passwd"                  — path traversal characters
//   - strings.Repeat("a", 65)          — exceeds the 64-byte ceiling
//
// 在 store 入口（parse / list / append / detail handler）做边界校验，
// 防止 runs/<jobID>/ 下意外文件名（temp file、备份）污染 List 输出，
// 也允许 HTTP 层在请求入口直接拒绝非法 ID 而不必下沉到磁盘 IO。
// R221-FIX-P1-2 + R234-CR-10（godoc 改写为输入形态描述，不再引用
// 私有的 generateRunID / generateID）+ R249-CR-23（补 Accepts/Rejects
// 示例，明确大写 hex 一律拒绝）。
//
// R249-ARCH-26 (#990): co-located with generateID / generateRunID here in
// job.go (the ID-spec home) rather than runstore.go — store / parse / HTTP
// callers all consume it, so its home is the ID schema, not the run store.
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
// LastRunAt / LastResult / LastError. Maintained on every finishRun (RFC
// §3.2) so the dashboard list endpoint can show terminal-state tallies
// without rescanning runs/<jobID>/. EWMA / P² latency aggregates landed
// in P1; the byte schema here stays the same.
//
// R239-CR-7: relocated from runinflight.go (where it was misleadingly co-
// located with the in-flight tracker) to sit next to the rest of Job's
// wire schema. runinflight.go is for live tick state; this is durable
// per-Job state.
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

// cronParseOptions is the single source of truth for the field set the cron
// schedule grammar accepts: standard 5-field (Minute/Hour/Dom/Month/Dow) plus
// @descriptors (@daily, @every 5m, …). Hoisted out of the cronParser var
// initialiser (R249-ARCH-24 / #988) so the accepted-field bitmask is a named,
// documented constant rather than a magic literal buried in a package-var
// init — the field set is now stated once and any future widening (e.g.
// adding robfigcron.Second) changes exactly this constant. A full move onto a
// Scheduler field seeded from cfg still needs design (touches scheduler.go),
// but this localises and names the binding as the first behaviour-preserving
// step.
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

// schedulePeriod 估算给定 cron 表达式在参考时刻 now 附近的周期（相邻两次
// 触发的间隔）。通过 sched.Next 两次外推实现，精度对 "每 N 分钟 /
// 每天 HH:MM" 这类常见形态足够。无法解析 / 不等间隔（DST 切换窗口）
// 时返回 0，调用方自行决定 fallback。
//
// now 必须由调用方显式提供，保证和上层 HasMissedSchedule /
// previousTickBefore 读取的"现在"完全同步——避免在 DST 切换或 NTP 校
// 正瞬间两者跨越不同小时，导致 period 估成 23h/25h 而产生 missed 假
// 判定。applyJitter 不在意这种纳秒级 skew，传 time.Now() 即可。
func schedulePeriod(schedule string, now time.Time) time.Duration {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return 0
	}
	return schedulePeriodFromSched(sched, now)
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
//
// R249-CR-10 (#954): 这是包内 unexported 的字符串入口帮助函数，唯一调用者是
// missed_test.go——生产路径全部走 previousTickBeforeFromSched（HasMissedSchedule
// 已 Parse 一次后复用 sched，避免重复正则）。保留它是为了让测试能用字符串
// schedule 直测回推逻辑，并非有跨包消费者（unexported 不可能被其他包用）。
func previousTickBefore(schedule string, now time.Time) time.Time {
	// R246-PERF-4: previously this called schedulePeriod(schedule, now)
	// which re-Parses the same expression we already parsed above.
	// cronParser.Parse is the dominant cost in this hot path
	// (HasMissedSchedule fans out across all jobs on every dashboard
	// list / metrics tick); folding the two calls into one Parse + a
	// FromSched helper is a free win.
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
// 避免在 HasMissedSchedule 路径上重复 Parse。R238-PERF-2。
//
// R250-CR-6 (#1139): schedulePeriod 是包内 unexported 帮助函数（不是“公开
// 签名”——没有跨包消费者）。当前唯一生产调用点是 applyJitter（scheduler_run.go
// 的 entryID==0 fallback 路径），加上包内 jitter_test.go。保留它是因为字符串
// 入口仍被 fallback 路径使用，并非“其他包测试有用”——旧注释的措辞已纠正。
func schedulePeriodFromSched(sched robfigcron.Schedule, now time.Time) time.Duration {
	first := sched.Next(now)
	second := sched.Next(first)
	return second.Sub(first)
}

// previousTickBeforeFromSched 同 previousTickBefore，但接受已解析的 sched +
// 已知 period，避免在 HasMissedSchedule 路径上重复 Parse / 重复估算 period。
// 上限守卫与 previousTickBefore 保持一致：previousTickMaxIter（1000）足以覆盖
// 任何合法 cron schedule 在 3×period 窗口内的迭代次数。R238-PERF-2。
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
// 性能：单次 cronParser.Parse + 1×schedulePeriodFromSched + 1×previousTickBeforeFromSched，
// 比走公开 schedulePeriod / previousTickBefore 路径少 2 次正则 Parse。R238-PERF-2。
//
// 关联：docs/rfc/cron-v2-polish.md §3.3 Increment C。
func HasMissedSchedule(j *Job, now, startedAt time.Time) (bool, time.Time) {
	return hasMissedScheduleImpl(j, nil, now, startedAt)
}

// HasMissedScheduleCached is the alloc-free variant of HasMissedSchedule for
// the dashboard 1Hz handleList fanout (R241-PERF-3 / #477). When the caller
// holds a *Job whose registerJob has run, j.cachedSched is non-nil and the
// helper skips the cronParser.Parse regex (the dominant cost at 50 jobs/s).
// Falls back to the parse path when the cache is cold (test fixtures, jobs
// loaded via JSON without registerJob, transient registerJob failure) so
// behaviour matches HasMissedSchedule on every input.
//
// The parse-saving optimisation is otherwise identical to HasMissedSchedule:
// same suppression window, same period derivation, same prev-tick guard,
// same return shape. Document changes there propagate here.
func HasMissedScheduleCached(j *Job, now, startedAt time.Time) (bool, time.Time) {
	if j == nil {
		return false, time.Time{}
	}
	return hasMissedScheduleImpl(j, j.cachedSched, now, startedAt)
}

// hasMissedScheduleImpl is the shared body of HasMissedSchedule and
// HasMissedScheduleCached. cached, when non-nil, lets the caller skip the
// regex parse; on cold cache the caller passes nil and we fall through to
// cronParser.Parse so test fixtures keep working.
func hasMissedScheduleImpl(j *Job, cached robfigcron.Schedule, now, startedAt time.Time) (bool, time.Time) {
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
	period := schedulePeriodFromSched(sched, now)
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
		if !j.CreatedAt.IsZero() && now.Sub(j.CreatedAt) > period {
			return true, prev
		}
		return false, time.Time{}
	}
	if prev.Sub(j.LastRunAt) > period*missedScheduleSlackNum/missedScheduleSlackDen {
		return true, prev
	}
	return false, time.Time{}
}

// validateSchedule checks if the cron expression is valid and respects the minimum interval.
//
// loc is the timezone in which the schedule will eventually be evaluated by the
// scheduler. R20260527122801-CR-7 (#1321): historically `time.Now()` (Local)
// was used to seed the interval probe, while registerJob registers the entry
// with WithLocation(s.location). On DST transitions or month-end "every N
// months" forms the two reference frames disagree — a schedule could pass the
// minCronInterval floor here but actually fire faster under cfg.Location.
// Pass the scheduler's effective location so the validation seed and runtime
// match. nil falls back to time.Local for the legacy free-standing path
// (tests / pre-Scheduler bootstraps that don't have a location yet).
func validateSchedule(schedule string, loc *time.Location) error {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return err
	}
	if loc == nil {
		loc = time.Local
	}
	// Check that the interval between the first two runs is at least minCronInterval.
	//
	// R249-CR-22 (#965): seed the interval probe from a FIXED reference instant
	// rather than time.Now(). With time.Now() the two-Next probe occasionally
	// straddled a DST transition — e.g. a spring-forward run made a genuine
	// every-5-minutes schedule appear ~1h apart (or a fall-back run inflated an
	// hourly schedule), so the minCronInterval floor would mis-classify the
	// interval depending on the wall-clock minute the operator happened to save
	// the job. A fixed mid-January noon is DST-quiet in every IANA zone (no zone
	// transitions occur at 2024-01-15 12:00 local), so the probe measures the
	// schedule's intrinsic interval deterministically. Anchored in loc so the
	// validation frame still matches the runtime WithLocation(loc) registration
	// (#1321): the date is interpreted in the operator's timezone, only the
	// instant is pinned away from transition boundaries.
	ref := time.Date(2024, time.January, 15, 12, 0, 0, 0, loc)
	first := sched.Next(ref)
	second := sched.Next(first)
	// R236-QA-07: drop the `interval > 0` guard. Previously a degenerate
	// schedule whose second tick equaled (or preceded) the first — interval
	// == 0 or negative — slipped past the floor and would fire as fast as
	// the dispatcher could observe the tick. minCronInterval is a positive
	// constant (5m) so `interval < minCronInterval` correctly rejects 0,
	// negatives, and anything below the floor in one expression.
	if interval := second.Sub(first); interval < minCronInterval {
		return fmt.Errorf("interval %v is too short, minimum is %v", interval, minCronInterval)
	}
	return nil
}
