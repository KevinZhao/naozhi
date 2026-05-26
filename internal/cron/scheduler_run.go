// scheduler_run.go: cron run execution path.
//
// Contains the heart of cron: executeOpt (the 344-line state machine that
// CASes the inflight gate, jitters, snapshots Job fields, runs the
// fresh-context preflight, spawns/sends through the session router,
// guards send with a deadline watchdog, and routes terminal state to
// finishRun) plus its helpers — preflight, snapshot, watchdog, error
// classification, jitter, and the per-job runInflight allocator.
//
// Split out of scheduler.go to keep the run-time hot path (which adds
// Spans, error classes, and observability hooks more often than any other
// area of cron) in one place and isolate it from CRUD / persist / notify
// concerns. No behaviour change; methods stay on *Scheduler so private
// fields remain accessible without exporting.

package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"

	robfigcron "github.com/robfig/cron/v3"
)

// cronSlowThreshold is the wall-clock budget beyond which a successful
// cron execution is counted as "slow" (metrics.CronExecutionSlowTotal).
// 30s is picked as an order-of-magnitude above a typical interactive
// agent turn; jobs that regularly tip over are candidates for timeout /
// workflow inspection. R208-OBS1.
const cronSlowThreshold = 30 * time.Second

// spawnElapsedWarnRatio is the fraction of jobTimeout the spawn phase
// (router.GetOrCreate) is allowed to consume before we emit the
// "send budget exceeds job/2" warning + bump CronSendBudgetDoubledTotal.
//
// 0.5 chosen because once spawn alone has consumed half the per-run
// budget, the in-flight wall clock can reach ~2*jobTimeout (spawn +
// fresh-budget Send), which is the doubling pattern operators of 300s+
// jobs need a runbook signal for. Lower the ratio (e.g. 0.4) to surface
// near-doubling earlier; raise (e.g. 0.7) to suppress noise on cold
// fresh-context runs that legitimately spawn slowly. R247-CR-28.
const spawnElapsedWarnRatio = 0.5

// executeIfNotDeletedOrPaused is the TriggerNow dispatch entry. It looks
// up the freshest *Job under s.mu.RLock, then — only if still present and
// not paused — releases the lock and calls executeOpt(cur, true). Deleted
// or paused jobs surface as a Debug-log skip with no run record.
//
// LOCK: caller MUST NOT hold s.mu (this acquires s.mu.RLock); robfig/cron
// MUST NOT hold its internal cron lock when invoking this (the snapshot →
// release → executeOpt split exists precisely so executeOpt's long-running
// send/notify pipeline never runs under s.mu).
//
// R238-GO-9 (#801): TriggerNow's goroutine bypasses the robfig/cron chain's
// Recover wrapper that protects the scheduled-tick path, so a panic in
// executeOpt would propagate up to the TriggerNow goroutine and kill it
// (and any inflight defer that hadn't fired yet — the deferred Done in
// scheduler_jobs.go's TriggerNow closure DOES still fire because it's
// registered before this call, but the panic surfaces as a runtime crash
// in the slog of the goroutine that ran it). Recover here so a panicking
// job fails loud once (Error log + stack) and the surrounding goroutine
// still completes. The scheduled path keeps robfig's Recover and does NOT
// pass through this helper — registerJob's AddFunc closure routes through
// executeJobIDIfLive directly so we don't double-recover.
func (s *Scheduler) executeIfNotDeletedOrPaused(jobID string) {
	defer func() {
		if r := recover(); r != nil {
			recordTriggerNowPanic(jobID, r)
		}
	}()
	s.executeJobIDIfLive(jobID, true /* viaTriggerNow */, "TriggerNow")
}

// recordTriggerNowPanic logs a TriggerNow-path panic. Split out of
// executeIfNotDeletedOrPaused's defer so the recover site stays a one-
// liner and the formatted-log path is exercisable in tests without
// deferring inside the test body. R238-GO-9 (#801).
func recordTriggerNowPanic(jobID string, r any) {
	slog.Error("TriggerNow: panic recovered, run abandoned",
		"job_id", jobID,
		"panic", r,
		"stack", string(debug.Stack()))
}

// executeJobIDIfLive is the shared lookup-and-dispatch primitive used by
// both TriggerNow (executeIfNotDeletedOrPaused) and the registerJob
// AddFunc closure (R247-CR-10). Both paths previously open-coded the
// RLock → exists/paused check → executeOpt fan-out with only the
// viaTriggerNow flag and Debug log subject differing; the duplicated
// closure made it easy to drift one path's pre-flight gate without the
// other. logSubject is the caller-supplied prefix used in skip Debug
// logs so operators distinguish "TriggerNow:" vs "cron:" in the
// shutdown / pause race traces.
func (s *Scheduler) executeJobIDIfLive(jobID string, viaTriggerNow bool, logSubject string) {
	s.mu.RLock()
	cur, ok := s.jobs[jobID]
	paused := ok && cur.Paused
	s.mu.RUnlock()
	if !ok {
		slog.Debug(logSubject+": job deleted before execute, skipping", "job_id", jobID)
		return
	}
	if paused {
		slog.Debug(logSubject+": job paused concurrently, skipping", "job_id", jobID)
		return
	}
	s.executeOpt(cur, viaTriggerNow)
}

// cleanupRunningJobIfIdle drops the s.runningJobs entry for jobID iff
// the runInflight CAS gate is currently false (no in-flight execute()
// holds it). R242-ARCH-15 (#758): the prior policy was "never clean,
// bounded by maxJobsHardCap=500" — but a long-lived deployment that
// adds and deletes thousands of cron jobs over weeks accumulates an
// unbounded sync.Map of dead *runInflight structs, never freed.
//
// The original ID-reuse split-CAS concern (a fresh AddJob colliding on
// the same 16-hex-char ID while the old execute() still holds the
// pointer to the OLD guard) is mitigated three ways at this entry
// point:
//
//  1. We only LoadAndDelete when running.Load() == false. If the gate
//     is held, leave the entry alone — the executeOpt goroutine still
//     holds the pointer and is about to releaseRun() on it. The caller
//     can re-attempt cleanup later (or accept the per-job-id leak;
//     bounded by jobs that get deleted while a run is in flight, an
//     even narrower window than the original maxJobsHardCap bound).
//  2. ID generation is crypto/rand 8 bytes (16 hex chars, 2^64 space);
//     for the maxJobsHardCap=500 working set the birthday-paradox
//     collision probability is ~2^-32 over the entire process lifetime
//     — far below any other production race we accept.
//  3. AddJob already retries on s.jobs collision (10 attempts, slog.Warn
//     each) so a re-used ID would not silently slip in undetected; the
//     window where new AddJob lands BEFORE the old run finishes is
//     vanishingly thin.
//
// Returns true if the entry was deleted. Safe to call after s.mu is
// released — sync.Map.LoadAndDelete needs no scheduler lock. Callers
// invoke this from postCleanup branches that already run lock-free.
func (s *Scheduler) cleanupRunningJobIfIdle(jobID string) bool {
	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return false
	}
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		// Defensive: an unexpected map value type implies the package
		// invariant was violated upstream. LoadAndDelete still cleans it.
		s.runningJobs.LoadAndDelete(jobID)
		return true
	}
	if inf.running.Load() {
		// In-flight execute() goroutine still holds the pointer and is
		// about to releaseRun(); skip — leaking THIS one entry until the
		// next DeleteJob sweep is cheaper than risking a CAS-gate split
		// against a (vanishingly rare) ID-reuse collision.
		return false
	}
	s.runningJobs.LoadAndDelete(jobID)
	return true
}

// jobInflight returns a lazily created *runInflight per job ID. The
// embedded atomic.Bool keeps the original CAS-gate semantics (used by
// executeOpt to reject concurrent runs); the surrounding metadata fields
// expose RunID/StartedAt/Phase to the list API for the cron-run-history
// P0 visibility work.
//
// Entries are reclaimed on DeleteJob via cleanupRunningJobIfIdle when
// the CAS gate is idle (R242-ARCH-15 / #758). The prior never-cleanup
// policy was a worst-case bound of maxJobsHardCap=500 entries; in long-
// lived deployments that delete and re-add jobs the working set could
// grow without limit.
func (s *Scheduler) jobInflight(id string) *runInflight {
	if v, ok := s.runningJobs.Load(id); ok {
		if inf, ok := v.(*runInflight); ok && inf != nil {
			return inf
		}
	}
	guard := &runInflight{}
	actual, _ := s.runningJobs.LoadOrStore(id, guard)
	if inf, ok := actual.(*runInflight); ok && inf != nil {
		return inf
	}
	// Should be unreachable given LoadOrStore's contract, but never return
	// nil to callers — they immediately call methods on the result.
	return guard
}

// jobSnapshot captures the mutable Job fields executeOpt reads under s.mu so
// the long-running send/notify pipeline can run without holding the lock.
// Snapshot is taken once after the rate-limit/jitter gate and reused for the
// rest of the execution; concurrent SetJobPrompt/UpdateJob therefore land
// for the next tick rather than racing the in-flight result. The shape
// mirrors the original inline reads — no fields added/removed.
//
// R247-CR-16: 字段按 size DESC 排，消除 string/bool/*bool 混排引入的 padding。
type jobSnapshot struct {
	prompt  string
	workDir string
	jobID   string
	// label is the human-readable title for IM notice prefixes (R233B-CR-4 /
	// R233B-CR-5). Computed via jobTitleOrFallback under s.mu so a
	// concurrent SetJobPrompt cannot tear Title vs Prompt-derived fallback.
	// Empty when both Title and Prompt are blank — deliverNotice falls back
	// to jobID in that case so the IM prefix never collapses to "[Cron ]".
	label      string
	platName   string
	chatID     string
	notifyPlat string
	notifyChat string
	schedule   string
	backend    string // "" = router default
	// lastSessionID 是 snapshot 时刻 Job.LastSessionID 的拷贝，供 fresh-
	// preflight 的 stub-refresh 闭包使用。R239-PERF-13: 闭包以前在每次
	// 失败回调时再开 s.mu.RLock 读 s.jobs[jobID].LastSessionID，新增本字段
	// 后 refresh 可直接调 registerStubByValue 不再触锁。语义保留——失败路径
	// 用 snap-time chain anchor（与本次 attempt 起点一致），后续新成功 run
	// 由其 finishRun 路径再覆写。
	lastSessionID string
	notify        *bool // nil = unset
	fresh         bool
}

// cronNoticePrefixFmt is the IM-notice prefix template every cron-side
// deliverNotice call funnels through. Centralising the literal closes
// R247-CR-5 (REPEAT-3): three execute-path notice strings each carried
// their own copy of "[Cron %s] …", so the only thing pinning the prefix
// shape was test fixtures grepping the formatted string. New notice
// sites should compose via formatCronNotice rather than inline a 4th
// copy.
const cronNoticePrefixFmt = "[Cron %s] %s"

// formatCronNotice renders the IM-notice line cron jobs send through
// deliverNotice. label is the snap.labelOrID() result (job title or
// fallback ID); body is the human-readable suffix already in the
// caller's display locale (Chinese for the static error templates,
// sanitised result text on the success path). Kept as a pure formatter
// so it can be reused from non-execute code paths (e.g. future manual
// retry surface) without dragging the deliverNotice / Scheduler
// dependencies along.
//
// R239-SEC-5: label flows through to the IM channel without ever
// transiting sanitiseRunResult, so an attacker-supplied job Title (e.g.
// "‮…" RLO) — which Scheduler.AddJob's MaxCronTitleLen check does
// not strip — would land verbatim in the IM render and reverse the
// surrounding text. Force it through osutil.SanitizeForLog (covers C0/C1,
// bidi overrides + isolates, LS/PS) so the rendered notice cannot be
// hijacked by control runes hidden in the title or prompt-derived
// fallback. body is already SanitizeForLog'd on the success path
// (sanitiseRunResult); applying it here is idempotent on clean ASCII
// templates and adds defence-in-depth.
func formatCronNotice(label, body string) string {
	// MaxCronTitleLen (256 runes) bounds label after the rune-count gate
	// at AddJob/UpdateJob — a 4× rune→byte budget is more than enough for
	// CJK / emoji to round-trip through SanitizeForLog without truncation.
	label = osutil.SanitizeForLog(label, MaxCronTitleLen*4)
	return fmt.Sprintf(cronNoticePrefixFmt, label, body)
}

// labelOrID returns the IM-notice display label: snap.label when populated,
// jobID otherwise. R233B-CR-5: keeps the "[Cron <X>] …" prefix readable
// without crashing on Title-empty + Prompt-empty edge cases.
func (s jobSnapshot) labelOrID() string {
	if s.label != "" {
		return s.label
	}
	return s.jobID
}

// snapshotJob reads j under s.mu so a concurrent SetJobPrompt /
// UpdateJob cannot tear the read across fields. Always returns a value
// (never nil); j is dereferenced inside the lock. RLock is sufficient
// since snapshotJob is read-only and runs from executeOpt outside s.mu.
//
// LOCK: Must NOT be called while s.mu is already held — acquires
// s.mu.RLock internally. robfig/cron callbacks must never hold s.mu
// when invoking snapshotJob (R227-CR-3).
func (s *Scheduler) snapshotJob(j *Job) jobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := jobSnapshot{
		prompt:        j.Prompt,
		workDir:       j.WorkDir,
		jobID:         j.ID,
		label:         jobTitleOrFallback(j),
		platName:      j.Platform,
		chatID:        j.ChatID,
		notifyPlat:    j.NotifyPlatform,
		notifyChat:    j.NotifyChatID,
		fresh:         j.FreshContext,
		schedule:      j.Schedule,
		backend:       j.Backend,
		lastSessionID: j.LastSessionID,
	}
	if j.Notify != nil {
		v := *j.Notify
		snap.notify = &v
	}
	return snap
}

// preflightArgs bundles the inputs to freshContextPreflightP0. R229-CR-8.
// Mirrors finishArgs's struct-bag pattern: the helper has 8 inputs that all
// flow through to the same finishRun/deliverNotice call sites and keeping
// them as positional args made future additions (e.g. a new error-class)
// risk silent argument-order swaps. Named fields also let tests express
// intent without reading parameter positions.
//
// R246-CR-014 (#757): field ordering is size DESC so the value type packs
// without intra-struct padding.
//
//	snap (jobSnapshot, ~144B incl. *Job/strings/bools — already size-DESC'd)
//	startedAt (time.Time, 24B)
//	notifyTo (NotifyTarget, 32B but two strings — keep with strings group)
//	16B fields (key, runID, trigger=string-typed) grouped
//	8B pointers (job, lg) trailing
//
// Pre-fix order (job → snap → key → lg → notifyTo → runID → startedAt →
// trigger) interleaved 8-byte pointers and 16-byte strings; on amd64/arm64
// each layout-mismatched neighbour added 8 bytes of padding, wasting
// ~16 bytes per executeOpt call (~1Hz × N jobs lifetime).
type preflightArgs struct {
	// snap 是 snapshotJob 拷贝出的快照（fresh / workDir / prompt /
	// jobID / labelOrID）。preflight 优先读 snap 而非 *job，避免与并发
	// DeleteJob/PauseJob 起读写竞争。Largest field — leads the struct so
	// no 8B head pads it down to a 16B / 24B boundary.
	snap jobSnapshot
	// startedAt 是 caller 进入 executeOpt 时记录的 wall-clock 起点；
	// finishRun 据此算 durationMS。preflight 失败也保留这个起点而非
	// 重新 time.Now()，让 dashboard 看到真实的"从触发到放弃"时长。
	startedAt time.Time
	// notifyTo 是 fresh-preflight 工作目录不可达分支用来回写
	// 「[Cron …] 工作目录不可达」中文提示的目标；其它失败分支不通知，
	// 因为「shutdown / Reset 失败」对终端用户没有可操作信号。Two strings
	// = 32B; sits with the 16B string-shaped fields below.
	notifyTo NotifyTarget
	// key 是 router GetOrCreate / Reset 用到的 session key
	// （`cron:<jobID>` 形式）。fresh 路径 Reset 该 key 后再让 caller
	// 重新 GetOrCreate，确保新 CLI 进程接管。
	key string
	// runID 是 caller 已生成的 16-char hex 运行 ID。失败分支转给
	// finishRun，使 cron_run_ended 与 cron_run_started 配对（emitOverlapSkipped
	// 同样模式）。
	runID string
	// trigger 区分 TriggerScheduled / TriggerManual；deliverNotice 与
	// dashboard run timeline 对二者渲染不同图标。Underlying type string
	// (16B) — packs with key/runID.
	trigger TriggerKind
	// job 是 freshContextPreflightP0 操作的目标 Job 指针（持有用于
	// stub-refresh 闭包），调用前 caller 已 snapshot；preflight 不会修改
	// *Job 字段，但失败分支会通过 finishArgs.job 把它转交给 finishRun。
	// 8B trailing — pointers grouped at the end.
	job *Job
	// lg 是带 jobID/runID 标签的 slog.Logger，preflight 自身只输出
	// info/warn 不输出 error（error 由 finishRun 的 errMsg 落盘统一处理）。
	lg *slog.Logger
	// finalizer 是 caller 栈上的 *runFinalizer。preflight 失败分支把它转
	// 交给 finishRun，让 cron_run_ended broadcast 之前 finalize 元数据，
	// CurrentRun(jobID) 与 broadcast 同步可见 ok=false。R246-GO-3 (#689).
	finalizer *runFinalizer
}

// freshContextPreflightP0 handles the fresh-mode prologue: ctx-cancel guard
// (CRON3), work-dir reachability check (CRON2), Reset, and the post-Reset
// existence re-check that prevents a leaked CLI process tied to a deleted
// job ("cron:<id>" orphan). Each failure branch records a (RunState,
// ErrorClass) tuple via finishRun so the run-history terminal protocol
// (broadcast cron_run_ended + counters + LastErrorClass write) participates.
//
// Returns:
//   - stubRefresh: closure that re-registers the sidebar stub on error
//     paths so the cron row stays visible. Caller invokes after error
//     branches; never invoke on success (live session owns the row).
//   - ok: false means the caller MUST return immediately. The helper has
//     already written the appropriate slog.Info/Warn + finishRun() for
//     the failure mode.
//
// In persistent mode (snap.fresh=false) the helper short-circuits with
// ok=true and a no-op stubRefresh so the caller's flow is uniform.
//
// R232-CR-7：原 preflightResult{stubRefresh: ...} 单字段 wrapper struct
// 已删除，直接返回二元组。
func (s *Scheduler) freshContextPreflightP0(args preflightArgs) (stubRefresh func(), ok bool) {
	snap := args.snap
	lg := args.lg
	noopRefresh := func() {}
	if !snap.fresh {
		return noopRefresh, true
	}
	if err := s.stopCtx.Err(); err != nil {
		lg.Info("cron fresh spawn suppressed during shutdown", "err", err)
		// Treat shutdown-cancel as canceled (not failed); skipPersist=true
		// preserves prior recordResult semantics where ctx-cancel did not
		// touch LastRunAt. The broadcast still emits so the dashboard sees
		// the run's terminal frame.
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
			skipPersist: true,
			prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		return noopRefresh, false
	}
	if !workDirReachable(snap.workDir) {
		lg.Warn("cron fresh spawn aborted: work_dir unreachable",
			"work_dir", snap.workDir)
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateFailed, errClass: ErrClassWorkDirUnreachable,
			errMsg: "work_dir unreachable",
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		s.deliverNotice(args.notifyTo, formatCronNotice(snap.labelOrID(), "工作目录不可达，本次执行已跳过。"))
		return noopRefresh, false
	}
	s.router.Reset(args.key)
	lg.Info("cron fresh context: session reset before run")
	// R239-PERF-13: refresh 闭包改用 snap 固化值直接调 registerStubByValue，
	// 不再每次失败回调时重开 s.mu.RLock 读 s.jobs[jobID]。snap 由
	// snapshotJob 在 RLock 下一次性拷贝（包括 LastSessionID），失败路径
	// 用这份 snap-time chain anchor 即可，后续新成功 run 由其 finishRun
	// 写新 LastSessionID 并由下一轮 snap 自然带入；闭包路径只是兜底让
	// sidebar 在失败后仍能渲染。仍需走 stillExists 校验：job 可能在
	// Reset 与本回调间隔内被 DeleteJob 删掉，那种情况下 stub 不应再注册。
	refresh := func() {
		s.mu.RLock()
		_, exists := s.jobs[snap.jobID]
		s.mu.RUnlock()
		if exists {
			s.registerStubByValue(snap.jobID, snap.workDir, snap.prompt, snap.lastSessionID)
		}
	}
	s.mu.RLock()
	_, stillExists := s.jobs[snap.jobID]
	s.mu.RUnlock()
	if !stillExists {
		lg.Info("cron job deleted mid-execute, skipping GetOrCreate")
		// Job deleted mid-execute: treat as canceled; no recordResult
		// (matches historical behaviour) but broadcast for visibility.
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateCanceled, errClass: ErrClassCanceled,
			errMsg: "job deleted mid-execute", skipPersist: true,
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		return refresh, false
	}
	return refresh, true
}

// executeOpt runs a cron job: send prompt to session, post result to chat.
// viaTriggerNow=true skips jitter delay (explicit user "run now" expects
// immediate execution); scheduled tick callers pass false.
//
// P0 cron-run-history (RFC §5):
//  1. CAS gate populates *runInflight with RunID/StartedAt/Trigger/Phase.
//  2. WS broadcast `cron_run_started` after CAS, before notify-target resolve.
//  3. Each error branch maps to a specific (RunState, ErrorClass) tuple via
//     finishRun, which:
//     - writes recordResult (LastResult/LastError/LastErrorClass + Counters)
//     - emits cron_run_ended broadcast
//     - bumps the per-state metrics.CronRun*Total counter
//     so all terminal paths share one observability hook.
//
// deadlineInterrupter is the narrow capability runDeadlineWatchdog needs
// from a session: a way to abort an in-flight CLI turn via the protocol's
// control_request channel. *session.ManagedSession satisfies this; cron
// tests stub it with a counting mock to assert the watchdog fired
// exactly when the deadline elapsed.
//
// SIGNATURE NOTE (R239-GO-2): InterruptViaControl here returns
// session.InterruptOutcome — DELIBERATELY different from the lower-level
// session.processIface.InterruptViaControl, which returns plain `error`.
// The two operate at different layers:
//
//   - processIface (internal/session) is the raw cli.Process facet — its
//     error reflects pipe-write / encode failure on the control_request
//     channel and tells nothing about whether the CLI actually had an
//     active turn to abort.
//   - ManagedSession.InterruptViaControl (which this interface mirrors)
//     wraps that and additionally classifies the no-active-turn / dead-
//     process / unsupported-by-backend cases into structured outcomes.
//     Cron's watchdog needs that classification to log "deadline fired,
//     interrupt did not land" vs "deadline fired, ACP backend unsupported".
//
// Refactor footgun: a future "InterruptViaControl" added anywhere on the
// session-facing surface MUST follow the layer convention — raw process =>
// error, managed session => InterruptOutcome. Do NOT collapse the two.
type deadlineInterrupter interface {
	InterruptViaControl() session.InterruptOutcome
}

// abortResult bundles the watchdog's exit signal: whether it actually
// fired the interrupt (i.e. the ctx ended via DeadlineExceeded, not via
// success-path Cancel) and what the InterruptViaControl outcome was when
// it did. The fired flag is the discriminator the caller logs.
type abortResult struct {
	outcome session.InterruptOutcome
	fired   bool
}

// runDeadlineWatchdog spawns a goroutine that waits on ctx and fires
// sess.InterruptViaControl exactly when ctx ends with DeadlineExceeded.
// The watchdog must run concurrently with sess.Send, NOT after — Send's
// internal defer flips Process.State Running→Ready the instant ctx fires,
// and InterruptViaControl gates on State==StateRunning, so calling it
// post-Send is dead code (returns ErrNoActiveTurn → outcome=no_turn).
//
// Channel contract (R249-CR-27): the returned channel has buffer=1 and
// is intentionally NOT closed. The goroutine self-completes thanks to
// buffer=1 — its single send never blocks, so the goroutine returns
// regardless of whether the caller reads. The caller drains ch only to
// observe the abort outcome (abort.fired / abort.outcome) for logging
// and to ensure InterruptViaControl has finished before recording the
// run state; failing to drain leaks the abortResult value, NOT the
// goroutine, and is harmless for shutdown bookkeeping. Earlier godoc
// said the caller "must drain" to keep the goroutine from outliving the
// run — that was misleading, what actually matters is sequencing the
// interrupt write before session.Reset on the next tick.
//
// On the success / non-deadline error path the caller cancels ctx
// explicitly; the watchdog observes ctx.Err()==Canceled, skips
// InterruptViaControl, and returns abortResult{fired:false}.
func runDeadlineWatchdog(ctx context.Context, sess deadlineInterrupter) <-chan abortResult {
	// R249-GO-3: defensive nil guard. A nil ctx would panic on <-ctx.Done()
	// inside the goroutine; a nil sess would panic on InterruptViaControl
	// when the deadline path fires. Both are caller bugs (production wires
	// real values), but the cron run goroutine swallows panics via
	// robfig/cron's recover chain elsewhere — here a panic would surface as
	// "cron logger" Error noise without the run ever recording a result.
	// Return a pre-completed channel so the caller's `<-abortCh` sees a
	// zero abortResult and proceeds with normal finishRun bookkeeping.
	// Buffer=1 with no close mirrors the success-path contract: the caller
	// drains exactly once; an unclosed channel of buffer=1 with one send
	// already buffered satisfies that without leaking a goroutine.
	if ctx == nil || sess == nil {
		ch := make(chan abortResult, 1)
		ch <- abortResult{}
		return ch
	}
	ch := make(chan abortResult, 1)
	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ch <- abortResult{outcome: sess.InterruptViaControl(), fired: true}
			return
		}
		ch <- abortResult{}
	}()
	return ch
}

// classifyExecError maps an error from GetOrCreate or Send to
// (RunState, ErrorClass) for finishRun. defaultClass distinguishes the
// session-spawn path (ErrClassSessionError) from the send path
// (ErrClassSendError); the helper unconditionally remaps the two
// context-derived sentinels:
//
//   - context.DeadlineExceeded → (RunStateTimedOut, ErrClassDeadlineExceeded)
//   - context.Canceled         → (RunStateCanceled, ErrClassCanceled)
//
// R241-ARCH-7: Canceled was historically handled by the caller via a
// dedicated `if errors.Is(err, context.Canceled)` branch ahead of this
// helper, so the state mapping was split across this site (DeadlineExceeded
// only) and the two caller blocks (Canceled / default). Folding Canceled
// into the helper keeps all (err → state, errClass) decisions in one
// place. Callers still own the side-effects that DIFFER per class
// (skipPersist=true for Canceled, operator-facing notice suppressed for
// Canceled, abort.fired logging on the send path) — see executeOpt's
// switch on errClass below for those policy choices.
//
// errors.Is order matters: context.Canceled wraps both genuine
// cancellation AND the "parent ctx cancelled mid-DeadlineExceeded" race
// where Send returns context.Canceled even though the deadline ticked
// first. Checking DeadlineExceeded first preserves the historical
// classification (deadline-exceeded WINS) so jobs that hit jobTimeout
// during a graceful shutdown still record RunStateTimedOut rather than
// RunStateCanceled. R230C-CR-7 (original) + R241-ARCH-7 (Canceled fold).
func classifyExecError(err error, defaultClass ErrorClass) (RunState, ErrorClass) {
	if errors.Is(err, context.DeadlineExceeded) {
		return RunStateTimedOut, ErrClassDeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return RunStateCanceled, ErrClassCanceled
	}
	return RunStateFailed, defaultClass
}

func (s *Scheduler) executeOpt(j *Job, viaTriggerNow bool) {
	// R20260526-GO-004: hot-path self-defence against a nil router. The
	// companion R20260526-GO-023 already logs at construction when
	// cfg.Router is nil, but tests build narrow fixtures via NewScheduler
	// without a router and only the fail-safe NPE-vs-skip distinction
	// matters at tick time. Without this guard the s.router.Reset call
	// inside freshContextPreflightP0 (line ~318) and s.router.GetOrCreate
	// (line ~712) NPE deep in the run loop, leaving CAS gates held and
	// triggerWG.Add already incremented. Short-circuit before any of that
	// state changes — the inflight CAS has not been taken yet, so an
	// early return is safe.
	if s == nil || s.router == nil {
		slog.Error("cron: router is nil; skipping run",
			"id", func() string {
				if j == nil {
					return ""
				}
				return j.ID
			}())
		return
	}
	// Guard against concurrent execution of the same job. The cron chain's
	// SkipIfStillRunning protects the scheduled-tick path, but TriggerNow
	// that arrives while a tick is in flight bypasses the chain entirely
	// (it calls execute directly when entryID == 0 or Run() on the entry
	// which is separately serialized). The per-job *runInflight (containing
	// the CAS atomic.Bool) keeps a uniform CAS gate while exposing run
	// metadata to the list API.
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		slog.Info("cron: job already running, skipping overlap", "job_id", j.ID)
		// Overlap is a skipped state (no LastRunAt update). Counters /
		// broadcast still fire so dashboards can surface the skip.
		s.emitOverlapSkipped(j, viaTriggerNow)
		return
	}
	// finalizer 是本次 run 的栈局部清理器。finishRun 在 emitRunEnded 之前
	// 调一次（让 broadcast 与 inflight view 同步可见 ok=false），下面的
	// defer 兜底覆盖 jitter-window 早返路径。done 标记由 finalize() 自身
	// 维护，run-A 的 defer 永远只看到 run-A 的 done=true，不会动到 run-B
	// 已抢占的 *runInflight 字段——并发隔离来自 finalizer 的 per-run 身份，
	// 不依赖 *runInflight 上的任何 atomic。R238-GO-2 + R246-GO-3 (#689).
	finalizer := &runFinalizer{inflight: inflight}
	defer func() {
		// R246-GO-3 (#689) 取代了 R246-CR-017 (#759) 把 reset + CAS-release
		// 抽到 inflight.releaseRun 的设计：那个共享 *runInflight 上的方法
		// 无法防 run-A 的迟到 defer clobber run-B 已抢占的字段。改用
		// per-run 栈局部 finalizer：done 标志保证 run-A 的 defer 只看到
		// 自己的 finalizer，绝不会动到 run-B 的元数据。reset → CAS-release
		// 的内部顺序（R238-GO-2）在 finalize() 内部保留。Gauge 的 -1 留在
		// 这里与 executeOpt 入口的 +1 视觉配对。
		finalizer.finalize()
		metrics.CronRunInflight.Add(-1)
	}()
	// R242-CR-14 (#706): metrics.CronRunInflight semantically tracks "how
	// many jobs hold the CAS slot right now", which is exactly the window
	// the defer guards. The historical placement was after metadata
	// population — fine when the only exit between defer and Add(+1) was
	// success, but the new generateRunID error branch returns before
	// reaching it, so the defer's Add(-1) would underflow the gauge.
	// Hoisting Add(+1) here pairs it with the defer's Add(-1) on every
	// early-return path (rand failure today, future preconditions later).
	metrics.CronRunInflight.Add(1)

	// Populate the inflight metadata under the CAS-true window. RunID is
	// generated once per run; StartedAt is captured before jitter so the
	// "running 12s" badge in the UI counts true wall-clock from CAS.
	runID, err := generateRunID()
	if err != nil {
		// R242-CR-14 (#706): crypto/rand 不可用时不能 panic 整个进程 ——
		// cron tick 是后台 goroutine，panic 会被 robfig/cron 的 wrapper
		// recover 但接下来这个 job 的 entry 也不会再正常工作；不如直接
		// log + skip 该次 tick，下一周期自然恢复（getrandom 失效是瞬时的
		// 内核事件）。defer 已经覆盖 inflight 释放 + CronRunInflight
		// 配对，无需手动清理。
		slog.Error("cron: failed to generate run ID; skipping tick",
			"job_id", j.ID, "trigger_now", viaTriggerNow, "err", err)
		return
	}
	startedAt := time.Now()
	trigger := TriggerScheduled
	if viaTriggerNow {
		trigger = TriggerManual
	}
	// R238-ARCH-3 (#742): single atomic.Pointer.Store with the complete
	// view replaces the prior 6 separate atomic.Pointer Stores. Readers
	// that snapshot() during this populate observe either the prior view
	// (still ok semantically — running gate already guards them) or the
	// complete new view; never a half-populated mix.
	inflight.populate(runInflightView{
		RunID:     runID,
		StartedAt: startedAt,
		Phase:     PhaseQueued,
		Trigger:   trigger,
	})
	// R247-GO-1: freshSnap is set authoritatively from snap.fresh after
	// snapshotJob runs under s.mu (line ~447); writing j.FreshContext here
	// without the lock was redundant and -race-suspect.
	// CronRunStartedTotal bumps inside emitRunStarted (R230C-GO-15).

	// Apply jitter after CAS, before snapshot. After-CAS so concurrent overlap
	// triggers are rejected immediately. Before-snapshot so an UpdateJob that
	// lands during the jitter window still lets the subsequent snapshot read
	// the new Prompt / WorkDir (matches the "edits take effect immediately"
	// operator expectation). TriggerNow skips jitter to preserve the
	// "run now = run now" semantics.
	if !viaTriggerNow && s.jitterMax > 0 {
		inflight.setPhase(PhaseJittering)
		// R250-GO-1: snapshot Schedule under s.mu.RLock so a concurrent
		// UpdateJob mutating j.Schedule doesn't race with applyJitter's
		// read. Mirrors the pattern used for the cur.Paused check below.
		//
		// R250-CR-14 (#1147): also snapshot j.entryID so we can fetch the
		// already-parsed robfigcron.Schedule via s.cron.Entry(entryID)
		// instead of re-parsing the schedule string inside applyJitter.
		// cronParser.Parse uses regex + struct alloc; on every tick of every
		// jittered job this was wasted work since robfig/cron already holds
		// the parsed Schedule for dispatch. Fall back to the string-parse
		// path (applyJitter) if entryID is 0 (job not yet registered, e.g.
		// tests) or if the entry has been removed concurrently (DeleteJob
		// races) — the parse-fallback preserves the historical behaviour.
		s.mu.RLock()
		schedStr := j.Schedule
		entryID := j.entryID
		var parsedSched robfigcron.Schedule
		if entryID != 0 {
			parsedSched = s.cron.Entry(entryID).Schedule
		}
		s.mu.RUnlock()
		if parsedSched != nil {
			applyJitterSched(s.stopCtx, parsedSched, s.jitterMax)
		} else {
			applyJitter(s.stopCtx, schedStr, s.jitterMax)
		}

		// R220-GO-3 + R246-GO-7: a DeleteJob OR a PauseJobByID that lands
		// during the jitter window must abort the run before we spawn /
		// send. The registerJob closure has a paused-check upstream of
		// executeOpt, but it runs *before* the jitter wait — a Pause that
		// lands inside the (default up-to-30s) jitter window would
		// otherwise leak through and violate the "Paused job must not run"
		// invariant. DeleteJob also leaves the inflight CAS still held
		// until we finish — blocking TriggerNow for the same id with an
		// "already running" overlap skip; the early return below releases
		// it via the deferred inflight.running.Store(false) above.
		// snapshotJob reads under s.mu so a stale dereference is
		// impossible after Delete (the field reads return the last-known
		// values and we never use them past this point).
		s.mu.RLock()
		cur, stillRegistered := s.jobs[j.ID]
		paused := stillRegistered && cur.Paused
		s.mu.RUnlock()
		if !stillRegistered {
			slog.Debug("cron: job deleted during jitter window, aborting run",
				"job_id", j.ID, "run_id", runID)
			return
		}
		if paused {
			slog.Debug("cron: job paused during jitter window, aborting run",
				"job_id", j.ID, "run_id", runID)
			return
		}
	}

	// Snapshot mutable Job fields once under s.mu so the rest of the
	// execution can run lock-free; concurrent SetJobPrompt/UpdateJob land
	// for the next tick rather than racing this in-flight result.
	snap := s.snapshotJob(j)
	inflight.setFresh(snap.fresh)

	// Resolve the effective notification target. Returns empty struct
	// when no delivery should happen, so both success and failure paths
	// below can call notify*() unconditionally-guarded by IsSet().
	notifyTo := s.resolveNotifyTarget(snap.platName, snap.chatID, snap.notifyPlat, snap.notifyChat, snap.notify)

	// Broadcast started — placed after snapshot so the event carries the
	// effective fresh flag and after notifyTo resolution so server-side
	// hub locks aren't held while we read s.mu.
	s.emitRunStarted(RunStartedEvent{
		JobID:     snap.jobID,
		RunID:     runID,
		StartedAt: startedAt,
		Trigger:   trigger,
		Fresh:     snap.fresh,
	})

	// `lg` instead of `log` to avoid shadowing the standard `log` package
	// imported at the top of the file (R60-GO-M2).
	//
	// R238-PERF-2 / R245-PERF-7 (#849, #858): one slog.With per execution
	// allocates a 4-attr Logger handler chain. We deliberately keep this
	// pattern despite the alloc: (a) the chain is reused 20+ times below
	// (success Info + send-deadline Warn + session-error Error + the
	// finishRun routing fan-out), so amortised cost per use is sub-µs;
	// (b) Caching on *Job would require invalidation on every
	// SetJobPlatform / SetJobChatID mutation — a correctness liability
	// disproportionate to ~200 ns saved per cron tick; (c) Caching on
	// *runInflight or jobSnapshot is per-execution scope, identical to
	// the local `lg` and only adds an indirection. Lazy build via
	// sync.Once would not help because line below unconditionally
	// triggers the alloc on first .Info call. The cron-tick path's hot
	// allocs are dominated by snapshot copy + CLI subprocess spawn —
	// optimising the logger here would not move the needle. Leave the
	// alloc; document the rationale so future reviewers don't reopen it.
	lg := slog.With("job_id", snap.jobID, "platform", snap.platName, "chat", snap.chatID, "run_id", runID)
	lg.Info("cron job executing", "prompt_len", len(snap.prompt))

	// Per-job timeout is always s.execTimeout (period scaling was removed —
	// robfig/cron's SkipIfStillRunning chain wrapper drops a colliding tick
	// instead of killing a long-running job, so the deadline does not need
	// to anticipate the next tick).
	jobTimeout := s.execTimeout
	// spawnCtx 是 GetOrCreate 阶段的超时上下文，从 GetOrCreate 返回后到
	// finishRun 之间这条 ctx 不再有任何消费者；让其底层 timer 一直挂到
	// executeOpt return 才被 defer 释放，意味着 N 个并发 in-flight job
	// (上限 maxJobsHardCap=500) 会在整个 Send 阶段 (≤jobTimeout) 占着
	// 等同的 *time.Timer 槽位。R250-GO-15 (#1078): 显式在 GetOrCreate
	// 出口 cancel()；defer 仍兜底（cancel 幂等，二次调用 no-op），早 free
	// 掉这条 timer 后续 ≤jobTimeout 都不再压 runtime timerproc。
	ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)
	defer spawnCancel()

	// s.agentCommands and s.agents are assigned once at scheduler
	// construction (cfg.AgentCommands / cfg.Agents) and never mutated;
	// reading them without s.mu is safe. If a future SetAgents API is
	// introduced both reads must move under s.mu.
	agentID, cleanText := session.ResolveAgent(snap.prompt, s.agentCommands)
	opts := cloneAgentOpts(s.agents[agentID])
	opts.Exempt = true // cron sessions must not count toward maxProcs or evict user sessions
	// Sprint 6c (docs/rfc/multi-backend.md §9): per-job backend override.
	// Empty snap.backend leaves opts.Backend untouched ("" already routes
	// through the router default, and the agent profile may have its own
	// backend pinned). A non-empty value wins because the user explicitly
	// picked it for this cron job from the dashboard. validateBackend at
	// the router boundary still rejects shape-invalid input (control chars,
	// overlength); unknown-but-well-formed backends fall back via wrapperFor.
	if snap.backend != "" {
		opts.Backend = snap.backend
	}
	if snap.workDir != "" {
		// Re-check allowedRoot at execute time to close the symlink-swap
		// race: validateWorkspace at creation resolved symlinks once, but
		// the target could have been retargeted since.
		//
		// R246-GO-12: when allowedRoot is set, hand the symlink-resolved
		// path to the cli wrapper rather than the raw snap.workDir. The
		// resolved path was just validated by EvalSymlinks; using it here
		// makes the validation view match the open view and forecloses a
		// final TOCTOU window between this check and the CLI's own open.
		// When allowedRoot is unset (sandbox disabled), keep the historical
		// filepath.Clean(snap.workDir) — workDirResolveUnderRoot's empty-
		// root short-circuit deliberately returns "" so we'd lose the
		// caller's workspace string.
		var workDirForCLI string
		if s.allowedRoot != "" {
			// R247-PERF-24: cached variant collapses repeated EvalSymlinks
			// for fast-firing jobs whose workDir / allowedRoot is stable.
			// TTL-bounded (workDirResolveCacheTTL) so a deliberate symlink
			// retarget surfaces within one notify-budget on the next tick.
			resolved, ok := s.workDirResolveUnderRootCached(snap.workDir)
			if !ok {
				lg.Warn("cron job work_dir outside allowed root; aborting run",
					"work_dir", snap.workDir)
				s.finishRun(finishArgs{
					job: j, runID: runID, startedAt: startedAt, trigger: trigger,
					state: RunStateFailed, errClass: ErrClassWorkDirOutsideRoot,
					errMsg: "work_dir outside allowed root",
					prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
					finalizer: finalizer,
				})
				return
			}
			workDirForCLI = resolved
		} else {
			workDirForCLI = filepath.Clean(snap.workDir)
		}
		opts.Workspace = workDirForCLI
	}
	key := session.CronKey(snap.jobID)

	// Fresh mode: drop any existing session (and its process + history) so
	// GetOrCreate spawns a brand-new CLI. The helper handles ctx-cancel,
	// workDir reachability, and post-Reset job-existence re-check. On
	// error paths the returned stubRefresh re-registers the sidebar row
	// so the cron entry doesn't vanish from the dashboard. On the success
	// path we skip stubRefresh because the live session carries its own
	// sidebar entry. Persistent mode short-circuits inside the helper
	// with a no-op stubRefresh.
	stubRefresh, ok := s.freshContextPreflightP0(preflightArgs{
		job: j, snap: snap, key: key, lg: lg, notifyTo: notifyTo,
		runID: runID, startedAt: startedAt, trigger: trigger,
		finalizer: finalizer,
	})
	if !ok {
		stubRefresh()
		return
	}

	inflight.setPhase(PhaseSpawning)
	// R250-CR-22 (#1155): capture spawnStart immediately before GetOrCreate
	// so the "send budget exceeds job/2" warn at line ~831 measures actual
	// spawn time, not (jitter + spawn). startedAt is captured pre-jitter for
	// the dashboard "running 12s" badge (true wall-clock from CAS), but the
	// warn is calibrated against jobTimeout/2 to detect when the spawn phase
	// alone consumed too much of the budget — folding jitter (default up to
	// 30s) into that measurement triggers false positives on healthy jobs
	// whose schedule landed unlucky in the jitter window.
	spawnStart := time.Now()
	sess, _, err := s.router.GetOrCreate(ctx, key, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent ctx cancelled mid-flight (graceful shutdown or job
			// deletion overlapping execute). The job will either be re-run
			// on the next tick or is intentionally gone; either way an IM
			// notification would be spam and the stored LastError would
			// falsely blame the job itself.
			lg.Info("cron session cancelled", "err", err)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true, // 与 historical recordResult skip 一致
				prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
				finalizer: finalizer,
			})
			stubRefresh()
			return
		}
		state, errClass := classifyExecError(err, ErrClassSessionError)
		if errClass == ErrClassDeadlineExceeded {
			lg.Info("cron session deadline exceeded", "err", err)
		} else {
			lg.Error("cron session error", "err", err)
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: "session error: " + err.Error(),
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: finalizer,
		})
		s.deliverNotice(notifyTo, formatCronNotice(snap.labelOrID(), "执行跳过，请稍后重试。"))
		stubRefresh()
		return
	}
	// R250-GO-15 (#1078): GetOrCreate consumed spawnCtx; nothing below references
	// it (Send uses sendCtx). Cancel now to free the underlying *time.Timer
	// instead of waiting for the function-scoped defer at executeOpt return.
	// On a 500-job-deep deployment with 5min jobTimeout this trims up to
	// ~500 idle timers off runtime timerproc during the Send window. The
	// outer defer remains as a safety net (cancel is idempotent: second
	// invocation is a no-op). Must come AFTER the err-return block above
	// so an early return on session error still trips the defer (already
	// covered) — placing here keeps the explicit cancel on the success
	// path only, mirroring the issue's "after the err handling" guidance.
	spawnCancel()

	// R242-ARCH-22 (#766): populate inflight.SessionID as soon as
	// GetOrCreate returns. Persistent-mode runs reuse a session that
	// already carries its CLI session_id (set during the original spawn's
	// init handshake), so sess.SessionID() is non-empty here. Fresh-mode
	// runs spawn a new CLI whose session_id is only stamped after the
	// init turn completes — sess.SessionID() returns "" in that window
	// and the post-Send setSessionID below remains the authoritative
	// write. Without this early capture, KnownSessionIDs / IsExcluded
	// probes during the Send window miss the in-flight run on
	// persistent-mode jobs (the auto-workspace-chain feature then
	// momentarily considers the cron session a candidate for prev_session_ids
	// until Send completes). setSessionID is idempotent and same-value
	// writes fast-path so the post-Send call is a no-op when the IDs match.
	if sid := sess.SessionID(); sid != "" {
		inflight.setSessionID(sid)
	}

	// R238-GO-4 / R236-GO-07 (#790, #500): Send is parented on s.stopCtx
	// so Scheduler.Stop() can short-circuit an in-flight cron Send instead
	// of letting it run for up to jobTimeout (default 5min) after Stop
	// returns — the historical Background parent created a use-after-free
	// class race where Send could write to a session that Router.Shutdown
	// had already reclaimed. The errors.Is(err, context.Canceled) branch
	// below already handles the cancel case with skipPersist=true, so a
	// Stop()-canceled Send no longer logs as a failure, no LastRunAt is
	// stamped, and the job re-runs on the next Start (matching the spawn
	// path's GetOrCreate cancel handling immediately above).
	//
	// R230B-GO-1 / R222-GO-1 (worst-case wall clock): the spawn ctx above
	// (line ~2062, derived from s.stopCtx with WithTimeout(jobTimeout)) and
	// this sendCtx do NOT share a budget — a slow GetOrCreate that consumes
	// most of jobTimeout still hands a fresh jobTimeout to Send below. A
	// pathological run can therefore last ~2*jobTimeout + a brief scheduling
	// gap before finishRun stamps a terminal state. This is intentional:
	// clamping sendCtx to (jobTimeout - time.Since(startedAt)) would amplify
	// flaky/cold-start spawns (~10s spawn → ~290s send budget on a 5min
	// job), turning a transient session-spawn slowdown into a user-visible
	// "send timed out" without the operator having any signal. The
	// scheduler-level overlap guard (robfig SkipIfStillRunning chain
	// wrapper) already prevents two concurrent
	// runs of the same job from stacking budgets, so the doubled wall
	// clock affects only the CURRENT run's recorded duration, not throughput.
	sendCtx, sendCancel := context.WithTimeout(s.stopCtx, jobTimeout)
	defer sendCancel()
	// R240-GO-4: emit an explicit signal when entering sendCtx after the
	// spawn phase already consumed >spawnElapsedWarnRatio of jobTimeout.
	// The wall-clock doubling described above is intentional but
	// historically silent; operators of 300s+ jobs need a structured
	// event to drive runbook alerts. Counter + slog pair (mirrors
	// CronExecutionSlowTotal + "cron execution slow" lower in this same
	// function). R247-CR-28: ratio extracted to a documented const so
	// future tuning is a one-line change with shared rationale.
	spawnWarnBudget := time.Duration(float64(jobTimeout) * spawnElapsedWarnRatio)
	// R250-CR-22 (#1155): time.Since(spawnStart) — not startedAt — so jitter
	// time is excluded. See spawnStart capture above (just before GetOrCreate).
	if spawnElapsed := time.Since(spawnStart); spawnElapsed > spawnWarnBudget {
		metrics.CronSendBudgetDoubledTotal.Add(1)
		// Message string preserved for runbook grep — see docs/ops/pprof.md
		// + internal/metrics/metrics.go CronSendBudgetDoubledTotal godoc.
		lg.Warn("cron send budget exceeds job/2",
			"job_id", snap.jobID,
			"spawn_elapsed_ms", spawnElapsed.Milliseconds(),
			"job_timeout_ms", jobTimeout.Milliseconds(),
			"send_budget_ms", jobTimeout.Milliseconds(),
			"warn_ratio", spawnElapsedWarnRatio)
	}
	inflight.setPhase(PhaseSending)

	// Watchdog: deadline-fired interrupt of the in-flight CLI turn. See
	// runDeadlineWatchdog for the rationale (must fire BEFORE Send returns,
	// otherwise Process.State has already flipped to Ready and
	// InterruptViaControl returns ErrNoActiveTurn → no-op).
	abortCh := runDeadlineWatchdog(sendCtx, sess)

	// Direct Send without sendWithBroadcast — cron jobs notify via onExecute callback instead.
	result, err := sess.Send(sendCtx, cleanText, nil, nil)
	// Cancel sendCtx so the watchdog returns promptly on the success / non-
	// deadline error path; on the deadline path it's already done. Block
	// on abortCh so the InterruptViaControl call (if any) completes before
	// we record the run state — otherwise a fast cron tick could overlap
	// the next session.Reset with the in-flight interrupt write.
	sendCancel()
	abort := <-abortCh
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Same rationale as the session-error branch above: suppress
			// the operator-facing notice so shutdown races don't look like
			// real failures. abort.fired can still be true here when a
			// stopCtx cancel races a near-deadline tick — surface it so
			// operators have a signal that an interrupt attempt happened
			// during the cancel path.
			lg.Info("cron send cancelled",
				"err", err,
				"abort_fired", abort.fired,
				"abort_outcome", abort.outcome)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true,
				prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
				finalizer: finalizer,
			})
			stubRefresh()
			return
		}
		state, errClass := classifyExecError(err, ErrClassSendError)
		if errClass == ErrClassDeadlineExceeded {
			// Log alongside the watchdog outcome so operators see both the
			// deadline AND whether the CLI was successfully interrupted in
			// the same line. ACP backends report "unsupported" here — we
			// accept the silent no-op since ACP cron jobs are rare and a
			// SIGINT fallback would couple two different abort semantics.
			//
			// R242-GO-7: when the watchdog fired but the interrupt did not
			// reach the CLI (outcome != InterruptSent and != InterruptUnsupported
			// for ACP), surface as Warn — the in-flight turn may still be
			// burning Send budget on the next tick, and operators need a
			// signal to investigate transport-level breakage. The
			// InterruptUnsupported tag is excluded by design: ACP jobs
			// always report unsupported and would otherwise spam Warn.
			if abort.fired && abort.outcome != session.InterruptSent &&
				abort.outcome != session.InterruptUnsupported {
				lg.Warn("cron send deadline exceeded; interrupt did not land",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			} else {
				lg.Info("cron send deadline exceeded",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			}
		} else {
			lg.Error("cron send error", "err", err)
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: "send error: " + err.Error(),
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: finalizer,
		})
		s.deliverNotice(notifyTo, formatCronNotice(snap.labelOrID(), "执行失败，请稍后重试。"))
		stubRefresh()
		return
	}
	if result.SessionID != "" {
		inflight.setSessionID(result.SessionID)
	}

	elapsed := time.Since(startedAt)
	lg.Info("cron job completed",
		"result_len", len(result.Text),
		"elapsed_ms", elapsed.Milliseconds())
	if elapsed > cronSlowThreshold {
		// R208-OBS1: poor-man's histogram — a single counter that fires
		// when a successful execution takes longer than cronSlowThreshold.
		// Wired here (not in finishRun) so only success-path latency
		// counts; error paths already surface via metrics state counters.
		metrics.CronExecutionSlowTotal.Add(1)
		lg.Warn("cron execution slow",
			"job_id", snap.jobID,
			"elapsed_ms", elapsed.Milliseconds(),
			"threshold_ms", cronSlowThreshold.Milliseconds())
	}
	// 把本次产生的 Claude session_id 也记下来：fresh_context=true 的
	// 路径下一次 Reset 会清掉 stub 的 chain，不保留这个 ID 的话
	// dashboard 点击 cron 侧边栏就看不到上一次的 JSONL 历史。
	// Send 路径的 result 帧总会带 SessionID（process.go 成功分支会填），
	// 传空只会出现在错误路径，finishRun 的 "" 分支自行短路。
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: trigger,
		state: RunStateSucceeded, sessionID: result.SessionID, result: result.Text,
		prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		finalizer: finalizer,
	})

	// R234-SEC-1: deliverNotice 必须用经过 sanitiseRunResult 的文本，
	// 否则未截断 / 未脱敏的 claude 输出会绕过所有保护落到 IM 渠道
	// （prompt-injection / IM 富文本指令 / 巨量响应耗尽队列）。
	// finishRun 在持久化路径已做过同样处理，这里复用相同管线。
	replyText := formatCronNotice(snap.labelOrID(), sanitiseRunResult(result.Text))
	s.deliverNotice(notifyTo, replyText)
}

// applyJitter 在执行 cron job 前引入一段随机延迟，用来把"整点共振起跑"的
// CPU / API 峰值打散。窗口上界 = min(jitterMax, period/4)：
//   - 5m 周期 → 最多抖 75s（不蚕食 1m 节奏）
//   - 30m 周期 → 最多抖 7m30s
//   - 1h+ 周期 → 抖满 jitterMax（默认 2m）
//
// 无法解析 schedule 或 period<=0 时用 jitterMax 兜底。抖动尊重 ctx：
// Stop() / 进程关机期间 stopCtx 取消 → 立即返回（不再执行 job）。
//
// 用 math/rand/v2（per-goroutine 安全且无全局锁），安全性不敏感：
// 这里的随机只影响启动时刻分布，不是密码学用途。
//
// R246-GO-22: NewTimer/defer Stop 在每次 tick 都分配 *time.Timer，
// 当前规模（~100 timer/min @ 100 jobs * 1Hz）成本可忽略，无需优化。
// 未来若 job 数突破 ~5000/min（≈ 80 alloc/s）再考虑 sync.Pool[*time.Timer]
// 或退化到 runtime.timeSleep 直接路径；提前优化只会让控制流更晦涩。
// time.After(d) 同样会 alloc *Timer 但不能被 Stop()，ctx 取消时会泄漏到
// 触发点为止，不适合此处。
func applyJitter(ctx context.Context, schedule string, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	// R250-CR-14 (#1147): the string-keyed entry point re-parses on every
	// call. Production now prefers applyJitterSched with the pre-parsed
	// robfigcron.Schedule pulled from s.cron.Entry, but this signature is
	// retained for tests and for the fallback path when entryID is 0 /
	// concurrently removed. Keep the parse → period → sleep pipeline
	// behaviourally identical to applyJitterSched so the two paths cannot
	// diverge.
	period := schedulePeriod(schedule, time.Now())
	jitterSleep(ctx, period, jitterMax)
}

// applyJitterSched is the entry point for the cron tick hot path. It reuses
// the already-parsed robfigcron.Schedule that the cron engine holds inside
// each Entry, avoiding a redundant cronParser.Parse on every tick. Behaviour
// is otherwise identical to applyJitter — same window cap (period/4), same
// jitterMax fallback, same ctx.Done() short-circuit. R250-CR-14 / #1147.
func applyJitterSched(ctx context.Context, sched robfigcron.Schedule, jitterMax time.Duration) {
	if jitterMax <= 0 {
		return
	}
	var period time.Duration
	if sched != nil {
		period = schedulePeriodFromSched(sched, time.Now())
	}
	jitterSleep(ctx, period, jitterMax)
}

// jitterSleep is the shared tail of applyJitter / applyJitterSched: clamp
// jitterMax by period/4 (with period<=0 meaning "use jitterMax as-is"),
// roll a random duration in [0, window), and sleep on a Timer that respects
// ctx cancellation. Extracted so the parse-once vs reuse-parsed split lives
// only in the two thin entry points above. R250-CR-14 / #1147.
func jitterSleep(ctx context.Context, period, jitterMax time.Duration) {
	window := jitterMax
	if period > 0 {
		if quarter := period / 4; quarter < window {
			window = quarter
		}
	}
	if window <= 0 {
		return
	}
	d := time.Duration(mrand.Int64N(int64(window)))
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// cloneAgentOpts returns a shallow copy of opts with all reference-typed
// fields (slices / maps) defensively cloned so downstream `append` /
// in-place writes cannot mutate the entry stored in Scheduler.agents.
//
// R246-GO-17 / R228-GO-P3-8: previous code only clipped ExtraArgs.
// Today AgentOpts only carries one slice field (ExtraArgs) — plus
// strings/bool — so clipping was sufficient. This helper centralises the
// clone so any future field added to session.AgentOpts (e.g. an Env map
// or HookConfigs slice) gets defensive copy automatically rather than
// leaking shared state into the per-run mutated copy. Keep this pure /
// allocation-light: it sits on the cron run hot path.
func cloneAgentOpts(opts session.AgentOpts) session.AgentOpts {
	if len(opts.ExtraArgs) > 0 {
		// Slice-clone (full copy) rather than three-index clip because the
		// caller may overwrite individual indices, not just append. Cost
		// dominated by the typical 0–3 args; negligible vs spawn syscalls.
		out := make([]string, len(opts.ExtraArgs))
		copy(out, opts.ExtraArgs)
		opts.ExtraArgs = out
	}
	return opts
}
