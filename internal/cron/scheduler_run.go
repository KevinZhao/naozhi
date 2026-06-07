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
	"log/slog"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"

	robfigcron "github.com/robfig/cron/v3"
)

// defaultCronSlowThreshold, spawnElapsedWarnRatio and minSendBudget are
// defined in tuning.go (R249-CR-16, #959), which collects all cron tuning
// knobs into one place with an operator-facing raise/lower table.

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

// stubRefresher carries the snap-time chain anchor (jobID + workDir + prompt
// + lastSessionID) that the error-path sidebar re-registration needs.
// R249-ARCH-25 (#989): freshContextPreflightP0 previously returned a bare
// `func()` closure that implicitly captured `snap`; the closure's lifetime
// and exactly which snap fields it pinned were invisible at the call site.
// Promoting it to a typed value with an explicit field set makes the captured
// state auditable (the four fields below are the entire dependency surface)
// and the zero value is a safe no-op — run() short-circuits when active is
// false, so the persistent-mode / early-bail paths need no special-casing.
//
// Unlike the R232-CR-7 single-field preflightResult wrapper that was removed,
// this struct carries the operation's actual value payload (not a lone func
// field), so it does not reintroduce that anti-pattern.
type stubRefresher struct {
	s             *Scheduler
	jobID         string
	workDir       string
	prompt        string
	lastSessionID string
	active        bool
}

// run re-registers the sidebar stub for the snapshotted job iff it still
// exists. The zero value (active=false) is an intentional no-op so callers
// invoke run() uniformly after both success-short-circuit and failure
// branches. stillExists is re-checked under s.mu because the failure callback
// may fire seconds after preflight returned, by which point DeleteJob could
// have removed the job — re-registering a stub for a deleted job would leak a
// phantom sidebar row. See the lock-pair contract at freshContextPreflightP0.
func (r stubRefresher) run() {
	if !r.active {
		return
	}
	r.s.mu.RLock()
	_, exists := r.s.jobs[r.jobID]
	r.s.mu.RUnlock()
	if exists {
		r.s.registerStubByValue(r.jobID, r.workDir, r.prompt, r.lastSessionID)
	}
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
func (s *Scheduler) freshContextPreflightP0(args preflightArgs) (stubRefresh stubRefresher, ok bool) {
	snap := args.snap
	lg := args.lg
	noopRefresh := stubRefresher{} // active=false → run() is a no-op
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
	if !s.workDirReachableCached(snap.workDir) {
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
	// R20260603140013-CR-3: containment early-check BEFORE the destructive
	// Reset below. resolveCronWorkspace (executeOpt) already aborts outside-root
	// runs, but it uses the TTL-cached workDirResolveUnderRootCached view; a
	// symlink retargeted outside allowedRoot within that TTL would pass there as
	// a stale-positive, letting us reach this point and blow away a live session
	// (Reset destroys the cron:<jobID> session + its process + history) for a
	// run that can never succeed. Re-validate with the uncached workDirUnderRoot
	// here so a freshly-retargeted symlink fails the run WITHOUT tearing down the
	// existing session. Mirrors resolveCronWorkspace's outside-root finishRun
	// (RunStateFailed / ErrClassWorkDirOutsideRoot) and the workDirReachable
	// branch's deliverNotice. noopRefresh leaves the sidebar stub untouched.
	if s.allowedRoot != "" && snap.workDir != "" &&
		!workDirUnderRoot(snap.workDir, s.allowedRoot, s.allowedRootResolved) {
		lg.Warn("cron fresh spawn aborted: work_dir outside allowed root",
			"work_dir", snap.workDir)
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateFailed, errClass: ErrClassWorkDirOutsideRoot,
			errMsg: "work_dir outside allowed root",
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		s.deliverNotice(args.notifyTo, formatCronNotice(snap.labelOrID(), "工作目录超出允许根目录，本次执行已跳过。"))
		return noopRefresh, false
	}
	// CRON1 / R194 (#401) — fresh-context atomicity invariant.
	//
	// Reset(key) here and the caller's subsequent GetOrCreate(key) (executeOpt
	// line ~1321) are two separate s.router (r.mu) acquisitions. A concurrent
	// rebuild landing in the gap could resurrect the cron:<jobID> session with
	// stale opts, bypassing fresh semantics. The concrete *session.Router DOES
	// expose an atomic primitive (ResetAndRecreate, router_lifecycle.go) but
	// the cron consumer interface (SessionRouter, scheduler.go) deliberately
	// does not surface it — correctness instead rests on a documented
	// single-writer invariant rather than a lock:
	//
	//   (1) cron↔cron: executeOpt is serialized per jobID by the inflight CAS
	//       gate (inflight.running.CompareAndSwap, executeOpt line ~947). A
	//       scheduled tick and a concurrent TriggerNow for the SAME job cannot
	//       both reach this Reset — the loser short-circuits at the CAS. This
	//       half is enforced in-package and pinned by the contract test in
	//       fresh_context_reset_atomic_test.go.
	//
	//   (2) cron↔external: the cron:<jobID> session-key namespace
	//       (sessionkey.CronKey) is reserved for the scheduler. Dashboard /
	//       IM sends route to channel-scoped keys (feishu:..., dashboard:...),
	//       never to a cron: key, so no external GetOrCreate races this Reset.
	//
	// If a future feature lets users send directly into a cron:<jobID>
	// session, invariant (2) breaks and this MUST migrate to the existing
	// router-level ResetAndRecreate primitive by adding it to the SessionRouter
	// interface and switching the preflight here — not by authoring a new one
	// (issue #401 Option A).
	s.router.Reset(args.key)
	lg.Info("cron fresh context: session reset before run")
	// R239-PERF-13: refresh 闭包改用 snap 固化值直接调 registerStubByValue，
	// 不再每次失败回调时重开 s.mu.RLock 读 s.jobs[jobID]。snap 由
	// snapshotJob 在 RLock 下一次性拷贝（包括 LastSessionID），失败路径
	// 用这份 snap-time chain anchor 即可，后续新成功 run 由其 finishRun
	// 写新 LastSessionID 并由下一轮 snap 自然带入；闭包路径只是兜底让
	// sidebar 在失败后仍能渲染。仍需走 stillExists 校验：job 可能在
	// Reset 与本回调间隔内被 DeleteJob 删掉，那种情况下 stub 不应再注册。
	//
	// R20260527-COR-12 (#1298) lock-pair contract：本函数对 s.jobs[snap.jobID]
	// 做两次 RLock 读，对应两个时间点：
	//
	//   (a) refresh closure 内（line ~490）：失败回调晚于本函数返回，闭包
	//       可能在数秒后才执行；那时 job 是否仍存在必须重读。
	//   (b) 紧随 Reset 之后（line ~497）：post-Reset 防御 — Reset 已经清空
	//       sessions/<key> 会话状态；如果 job 此刻已被 DeleteJob 删掉，
	//       本函数必须返回 ok=false 防止后续 GetOrCreate 重建一个 cron:<id>
	//       孤儿。
	//
	// 两次读独立、各自的 RLock 持锁窗口短小，且(a)只在(b)成功后才有机会
	// 触发，所以"重复读 snap.jobID"是设计意图而非 bug。Reviewer 看到第二
	// 次 RLock 时不要"合并优化"——会让(a)失去独立的 stillExists 检查。
	refresh := stubRefresher{
		s:             s,
		jobID:         snap.jobID,
		workDir:       snap.workDir,
		prompt:        snap.prompt,
		lastSessionID: snap.lastSessionID,
		active:        true,
	}
	// (b) post-Reset 存在性检查 — 见上文 lock-pair contract。
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
// applyJitterAndRecheck performs the post-CAS jitter sleep and the
// post-jitter delete/pause recheck for a scheduled (non-TriggerNow) run with
// jitter enabled. Extracted verbatim from executeOpt under R249-CR-1 (#945) /
// R238-ARCH-2 (#734) so the run path reads as a sequence of named phases
// rather than one ~340-line state machine; behaviour is unchanged.
//
// Returns:
//   - snap / snapTaken: when the recheck passes, snap is the under-RLock
//     snapshot of j and snapTaken is true so the caller skips the redundant
//     fall-through snapshotJob. On the abort paths snapTaken is false.
//   - abort: true means a DeleteJob / PauseJobByID landed during the jitter
//     window; the caller MUST return immediately (the deferred finalizer
//     releases the inflight CAS + gauge). The aborting slog.Debug is emitted
//     here so the caller's branch stays a bare `return`.
//
// Caller contract: only invoked when !viaTriggerNow && s.jitterMax > 0, and
// after inflight metadata is populated (so setPhase(PhaseJittering) is the
// correct transition).
func (s *Scheduler) applyJitterAndRecheck(j *Job, runID string, inflight *runInflight) (snap jobSnapshot, snapTaken bool, abort bool) {
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
	cachedPeriod := j.cachedPeriod
	var parsedSched robfigcron.Schedule
	if entryID != 0 && cachedPeriod <= 0 {
		// R242-PERF-2 (#664): only fetch the parsed Schedule for the live
		// computation when the cache is cold. registerJob populates
		// cachedPeriod alongside entryID, so production runs hit the
		// pre-computed branch and skip both the s.cron.Entry RLock-friendly
		// lookup and the 2× sched.Next that schedulePeriodFromSched runs.
		parsedSched = s.cron.Entry(entryID).Schedule
	}
	s.mu.RUnlock()
	switch {
	case cachedPeriod > 0:
		// R242-PERF-2 (#664): hot path — period was cached at registerJob
		// time, no per-tick parsing or sched.Next needed.
		jitterSleep(s.stopCtx, cachedPeriod, s.jitterMax)
	case parsedSched != nil:
		applyJitterSched(s.stopCtx, parsedSched, s.jitterMax)
	default:
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
	//
	// R20260528-PERF-2 (#1351): the snapshot copy is also taken under
	// the SAME RLock when the recheck passes — see snapshotJobLocked.
	// This eliminates the immediately-following second RLock the
	// pre-fix code paid via s.snapshotJob(j). The
	// `cur, stillRegistered` / `paused := ...` literal pattern stays
	// in this scope so
	// TestExecuteOpt_JitterPausedReCheck_SourceAnchor in jitter_test.go
	// continues to lock down the recheck against silent removal.
	s.mu.RLock()
	cur, stillRegistered := s.jobs[j.ID]
	paused := stillRegistered && cur.Paused
	if stillRegistered && !paused {
		snap = snapshotJobLocked(j)
		snapTaken = true
	}
	s.mu.RUnlock()
	if !stillRegistered {
		slog.Debug("cron: job deleted during jitter window, aborting run",
			"job_id", j.ID, "run_id", runID)
		return jobSnapshot{}, false, true
	}
	if paused {
		slog.Debug("cron: job paused during jitter window, aborting run",
			"job_id", j.ID, "run_id", runID)
		return jobSnapshot{}, false, true
	}
	return snap, snapTaken, false
}

// resolveCronWorkspace resolves the snapshot's workDir into the path handed to
// the CLI wrapper, re-validating the allowedRoot containment at execute time.
// Extracted verbatim from executeOpt under R238-ARCH-2 (#734) so the run path
// reads as a sequence of named phases; behaviour is unchanged.
//
// Re-check allowedRoot at execute time to close the symlink-swap race:
// validateWorkspace at creation resolved symlinks once, but the target could
// have been retargeted since.
//
// R246-GO-12: when allowedRoot is set, hand the symlink-resolved path to the
// cli wrapper rather than the raw snap.workDir. The resolved path was just
// validated by EvalSymlinks; using it here makes the validation view match the
// open view and forecloses a final TOCTOU window between this check and the
// CLI's own open.
//
// R242-SEC-10 (#638): when allowedRoot is unset (sandbox disabled) the in-root
// containment short-circuit returns "" so we fall back to a best-effort
// EvalSymlinks (not bare filepath.Clean, which does NOT resolve symlinks). A
// workDir like /var/cron-jobs/foo could point through a symlink at an
// operator-unintended location, and the CLI would then chdir there with the
// only validation being "looks lexically clean". An EvalSymlinks failure
// (broken link, missing target, or insufficient perms to traverse) falls back
// to the cleaned raw input rather than aborting the run — losing resolution is
// preferable to refusing to run when sandbox is already off by operator choice.
//
// Returns abort=true (after emitting finishRun for the outside-root failure
// class) only on the allowedRoot-set, containment-rejected path; the caller
// MUST return immediately. All other paths return abort=false with the
// resolved path.
func (s *Scheduler) resolveCronWorkspace(
	j *Job, snap jobSnapshot, runID string, startedAt time.Time,
	trigger TriggerKind, lg *slog.Logger, finalizer *runFinalizer,
) (workDirForCLI string, abort bool) {
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
			return "", true
		}
		// #1730: the cached gate above can pass on a stale-positive within
		// workDirResolveCacheTTL — an operator may point the workDir symlink at
		// an allowed path, let the cache warm, then retarget it outside
		// allowedRoot, and the next fresh=false tick would launch the CLI under
		// the retargeted path before the TTL expires. Re-run the uncached
		// workDirUnderRoot gate here to close that window, mirroring the
		// fresh-path containment check (RunStateFailed / ErrClassWorkDirOutsideRoot,
		// no subprocess launch). The extra EvalSymlinks is bounded to the
		// about-to-launch tick (minCronInterval >= 5min, not a hot path). On
		// success we keep the cached-resolved path rather than the uncached
		// result to avoid double-EvalSymlinks semantic divergence.
		if !workDirUnderRoot(snap.workDir, s.allowedRoot, s.allowedRootResolved) {
			lg.Warn("cron job work_dir outside allowed root (uncached recheck); aborting run",
				"work_dir", snap.workDir)
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateFailed, errClass: ErrClassWorkDirOutsideRoot,
				errMsg: "work_dir outside allowed root",
				prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
				finalizer: finalizer,
			})
			return "", true
		}
		return resolved, false
	}
	if resolved, err := filepath.EvalSymlinks(snap.workDir); err == nil {
		return filepath.Clean(resolved), false
	}
	return filepath.Clean(snap.workDir), false
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
		// R20260527122801-CR-13 (#1323): emit a synthetic started→ended
		// pair so dashboard "running" counters and subscriber timelines
		// stay consistent. errClass=router_missing distinguishes this
		// degraded short-circuit from a real overlap_skipped. Guarded on
		// non-nil s + j: if s is nil there's no Scheduler to broadcast
		// from, and a nil j means we have no JobID to attach the frames
		// to either.
		if s != nil && j != nil {
			s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassRouterMissing, "router unavailable", "router-missing")
		}
		return
	}
	// Guard against concurrent execution of the same job. The cron chain's
	// SkipIfStillRunning protects the scheduled-tick path, but TriggerNow
	// that arrives while a tick is in flight bypasses the chain entirely
	// (it calls execute directly when entryID == 0 or Run() on the entry
	// which is separately serialized). The per-job *runInflight (containing
	// the CAS atomic.Bool) keeps a uniform CAS gate while exposing run
	// metadata to the list API.
	//
	// R20260603140013-GO-2 (#1706): the jobInflight load and the CAS below
	// MUST be one atomic step relative to cleanupRunningJobIfIdle's
	// Load→CompareAndDelete. Holding the per-jobID gate across both closes the
	// TOCTOU window where a DeleteJob racing TriggerNow deletes the map entry
	// after we load the old *runInflight but before we CAS it — which would
	// leave us CASing an orphaned gate while a second executeOpt LoadOrStores
	// a fresh one, double-executing the job. The gate is released right after
	// the CAS — the heavy run body does not need it. cleanup takes the same
	// gate, so it can only see the gate as idle (we are not in this window) or
	// running (CAS won → cleanup's running.Load()==true → skip), never the
	// orphan-in-between.
	gate := s.jobGateLock(j.ID)
	gate.Lock()
	inflight := s.jobInflight(j.ID)
	won := inflight.running.CompareAndSwap(false, true)
	gate.Unlock()
	if !won {
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

	// R20260527122801-CR-8 (#1322): post-CAS paused/deleted recheck. The
	// callers (executeJobIDIfLive for TriggerNow and the registerJob
	// AddFunc closure for scheduled ticks) check paused/deleted under
	// s.mu.RLock and release the lock BEFORE invoking executeOpt. There is
	// a narrow 1-2µs window between that release and the CAS above where a
	// concurrent PauseJobByID / DeleteJobByID can land — the original
	// jitter-window recheck (line ~902-915 below) only fires in the
	// `!viaTriggerNow && s.jitterMax > 0` branch, leaving TriggerNow and
	// the jitter==0 scheduled path unprotected. Recheck once here, after
	// CAS but before any heavy work (snapshot / spawn / send), so a Pause
	// that lands in the cross-lock window aborts the run cleanly. The
	// post-CAS placement also subsumes the existing jitter-window recheck
	// for the in-jitter case — Pause that lands DURING jitter is still
	// caught by that block's recheck after the sleep returns. The defer
	// above handles inflight CAS release + gauge decrement on this early
	// return.
	{
		s.mu.RLock()
		curCAS, stillRegisteredCAS := s.jobs[j.ID]
		pausedCAS := stillRegisteredCAS && curCAS.Paused
		s.mu.RUnlock()
		if !stillRegisteredCAS || pausedCAS {
			// R243-ARCH-13 (#841): both cross-lock abort branches log the
			// same {job_id, trigger_now} pair; bind it once via slog.With.
			casLg := slog.With("job_id", j.ID, "trigger_now", viaTriggerNow)
			if !stillRegisteredCAS {
				casLg.Debug("cron: job deleted between dispatch lookup and CAS, aborting run")
				// R040034-CR-1 (#1410): emit synthetic started→ended pair so
				// dashboard subscribers see a complete lifecycle frame instead
				// of a 1-2µs gap when Delete lands in the cross-lock window.
				// Mirrors the router-missing precedent at the top of
				// executeOpt and the overlap-skipped emit on CAS-lost.
				s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassDeletedConcurrent, "job deleted between dispatch and CAS", "deleted-during-dispatch")
			} else {
				casLg.Debug("cron: job paused between dispatch lookup and CAS, aborting run")
				// R040034-CR-1 (#1410): see DeletedConcurrent above. Pause
				// landing in the cross-lock window also gets a synthetic pair
				// so the dashboard "running" counter stays consistent.
				s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassPausedConcurrent, "job paused between dispatch and CAS", "paused-during-dispatch")
			}
			return
		}
	}

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
	// R247-ARCH-11 (#643): the run's StartedAt anchors both the dashboard
	// "running 12s" badge and finishRun's DurationMS (endedAt - startedAt).
	// Read it via the injected clock so a fake clock can pin a deterministic
	// run duration end-to-end (startedAt here + endedAt in finishRun both flow
	// through s.now()). Default clock is time.Now(), byte-identical to the
	// prior inline read.
	startedAt := s.now()
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

	// snap is populated either inside the jitter block (folded RLock with
	// the post-jitter recheck — R20260528-PERF-2 / #1351) or by the
	// fall-through snapshotJob() call below. snapTaken tracks which path
	// won so we never double-snapshot — taking it twice would risk the
	// second read seeing a fresher UpdateJob than the recheck observed,
	// silently violating the "post-jitter snapshot reflects the same
	// instant the recheck verified" contract.
	var snap jobSnapshot
	var snapTaken bool

	// Apply jitter after CAS, before snapshot. After-CAS so concurrent overlap
	// triggers are rejected immediately. Before-snapshot so an UpdateJob that
	// lands during the jitter window still lets the subsequent snapshot read
	// the new Prompt / WorkDir (matches the "edits take effect immediately"
	// operator expectation). TriggerNow skips jitter to preserve the
	// "run now = run now" semantics.
	if !viaTriggerNow && s.jitterMax > 0 {
		var abort bool
		snap, snapTaken, abort = s.applyJitterAndRecheck(j, runID, inflight)
		if abort {
			return
		}
	}

	// Snapshot mutable Job fields once under s.mu so the rest of the
	// execution can run lock-free; concurrent SetJobPrompt/UpdateJob land
	// for the next tick rather than racing this in-flight result. The
	// jitter-enabled path already populated snap inside the recheck's
	// RLock window — skip the redundant call here.
	if !snapTaken {
		snap = s.snapshotJob(j)
	}
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
	// R238-PERF-2 / R245-PERF-7 / R242-PERF-3 (#849, #858, #666): one
	// slog.With per execution allocates a 4-attr Logger handler chain. We
	// deliberately keep this pattern despite the alloc: (a) the chain is
	// reused 20+ times below (success Info + send-deadline Warn +
	// session-error Error + the finishRun routing fan-out), so amortised
	// cost per use is sub-µs; (b) Caching on *Job would require
	// invalidation on every SetJobPlatform / SetJobChatID mutation — a
	// correctness liability disproportionate to ~200 ns saved per cron
	// tick; (c) Caching on *runInflight or jobSnapshot is per-execution
	// scope, identical to the local `lg` and only adds an indirection.
	// Lazy build via sync.Once would not help because line below
	// unconditionally triggers the alloc on first .Info call. The
	// cron-tick path's hot allocs are dominated by snapshot copy + CLI
	// subprocess spawn — optimising the logger here would not move the
	// needle. Leave the alloc; document the rationale so future reviewers
	// don't reopen it. #666 is the latest [REPEAT-N] — closing as
	// won't-fix-by-design.
	lg := slog.With("job_id", snap.jobID, "platform", snap.platName, "chat", osutil.SanitizeForLog(snap.chatID, 64), "run_id", runID) // R20260607-SEC-1: chatID is attacker-influenced; strip C1/bidi log-injection chars
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

	// agentCommands and agents are published once at scheduler construction
	// (cfg.AgentCommands / cfg.Agents) via configMapsPtr and never swapped
	// today; reading them lock-free through configMaps() is safe. A future
	// SetAgents/hot-reload API Store()s a fresh *cronConfigMaps so this read
	// stays race-free without moving under s.mu (R249-ARCH-27 / #991). Load
	// the snapshot once so both reads see the same generation.
	cm := s.configMaps()
	agentID, cleanText := resolveAgent(snap.prompt, cm.agentCommands)
	opts := cloneAgentOpts(cm.agents[agentID])
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
		workDirForCLI, abort := s.resolveCronWorkspace(j, snap, runID, startedAt, trigger, lg, finalizer)
		if abort {
			return
		}
		opts.Workspace = workDirForCLI
	}
	key := sessionkey.CronKey(snap.jobID)

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
		stubRefresh.run()
		return
	}

	// RNEW-003 (#423): the spawn phase (GetOrCreate + its cancel/error
	// classification + early inflight SessionID capture) is extracted to
	// executeGetSession so executeOpt's body has one fewer ctx-owning concern
	// and the spawnCtx lifecycle (GetOrCreate-only, cancelled at its exit)
	// reads as a single unit. Behaviour-preserving: the same finishRun +
	// stubRefresh + deliverNotice fire on each failure branch, and spawnStart
	// is returned for the downstream send-budget warn.
	sess, spawnStart, abortSpawn := s.executeGetSession(getSessionArgs{
		ctx: ctx, spawnCancel: spawnCancel, key: key, opts: opts,
		job: j, snap: snap, runID: runID, startedAt: startedAt, trigger: trigger,
		lg: lg, notifyTo: notifyTo, finalizer: finalizer,
		stubRefresh: stubRefresh, inflight: inflight,
	})
	if abortSpawn {
		return
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
	// R20260527122801-CR-2 (#1311): clamp sendCtx to the remaining budget
	// after spawn consumed `time.Since(spawnStart)` of jobTimeout, with a
	// minSendBudget floor to preserve the historical concern (R230B-GO-1 /
	// R222-GO-1) that flaky cold-start spawns shouldn't immediately surface
	// as "send timed out" — a 30s floor still lets a healthy Send complete
	// while bounding worst-case wall-clock to (jobTimeout + minSendBudget)
	// instead of ~2*jobTimeout. Operators previously saw systemd
	// TimeoutStopSec exceeded on cron runs because the un-clamped sendCtx
	// could double the per-run budget; the floor + spawnElapsedWarnRatio
	// warn (just below) keep the operator-signal path intact.
	sendBudget := jobTimeout - time.Since(spawnStart)
	if sendBudget < minSendBudget {
		sendBudget = minSendBudget
	}
	sendCtx, sendCancel := context.WithTimeout(s.stopCtx, sendBudget)
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
			// R20260527122801-CR-2 (#1311): send_budget now reflects the
			// post-clamp budget (jobTimeout - spawnElapsed, floored at
			// minSendBudget) instead of the historical un-clamped jobTimeout.
			// Operators reading this warn line can compare spawn_elapsed +
			// send_budget against jobTimeout to see the floor in action.
			"send_budget_ms", sendBudget.Milliseconds(),
			"warn_ratio", spawnElapsedWarnRatio)
	}
	inflight.setPhase(PhaseSending)

	// R215-ARCH-P2-5 (#581) partial: the Send + watchdog + abort-drain
	// trio is a self-contained sub-machine that doesn't need to share
	// stack frame with the surrounding executeOpt. Extracting it
	// localises the watchdog ↔ Send ordering contract (drain abortCh
	// AFTER cancelling sendCtx) so a future executeOpt split doesn't
	// accidentally reorder the lines and let the next Reset race the
	// in-flight interrupt write. See sendWithWatchdog godoc for the
	// invariant.
	result, abort, err := sendWithWatchdog(sendCtx, sendCancel, sess, cleanText)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Same rationale as the session-error branch above: suppress
			// the operator-facing notice so shutdown races don't look like
			// real failures. abort.fired can still be true here when a
			// stopCtx cancel races a near-deadline tick — surface it so
			// operators have a signal that an interrupt attempt happened
			// during the cancel path.
			//
			// R242-GO-7 (#555): mirror the DeadlineExceeded branch below —
			// when the watchdog fired but the interrupt did not land
			// (outcome neither InterruptSent nor InterruptUnsupported), the
			// in-flight turn may still be wedged at session level even
			// though the cron run is recorded as cancelled. Operators need
			// the Warn signal to investigate transport-level breakage in
			// the same shape as the deadline path; otherwise a "fired-but-
			// silent" interrupt during a cancel-deadline race is buried at
			// Info severity and slips past log alerts.
			if abort.fired && abort.outcome != InterruptSent &&
				abort.outcome != InterruptUnsupported {
				lg.Warn("cron send cancelled; interrupt did not land",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			} else {
				lg.Info("cron send cancelled",
					"err", err,
					"abort_fired", abort.fired,
					"abort_outcome", abort.outcome)
			}
			s.finishRun(finishArgs{
				job: j, runID: runID, startedAt: startedAt, trigger: trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true,
				prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
				finalizer: finalizer,
			})
			// R20260607-CORR-2: mirrors the #1829 success-path Reset (line ~1017).
			// Fresh-context sessions are Exempt=true so TTL cleanup skips them;
			// without an explicit Reset the CLI+MCP subtree (~1.6 GB) leaks until
			// the next tick's preflight (up to 24 h for @daily jobs).
			if snap.fresh {
				s.router.Reset(key)
			}
			stubRefresh.run()
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
			if abort.fired && abort.outcome != InterruptSent &&
				abort.outcome != InterruptUnsupported {
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
			// R20260603-SEC-4: sanitise before logging to strip IP:port / paths.
			lg.Error("cron send error", "err", sanitiseRunErrMsg(err.Error()))
		}
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: state, errClass: errClass, errMsg: "send error: " + sanitiseRunErrMsg(err.Error()), // R20260607-GO-004: strip IP:port/paths, mirrors lg.Error above
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: finalizer,
		})
		s.deliverNotice(notifyTo, formatCronNotice(snap.labelOrID(), "执行失败，请稍后重试。"))
		// R20260607-CORR-2: mirrors the #1829 success-path Reset (line ~1017).
		// Fresh-context sessions are Exempt=true so TTL cleanup skips them;
		// without an explicit Reset the CLI+MCP subtree (~1.6 GB) leaks until
		// the next tick's preflight (up to 24 h for @daily jobs).
		if snap.fresh {
			s.router.Reset(key)
		}
		stubRefresh.run()
		return
	}
	if result.SessionID != "" {
		inflight.setSessionID(result.SessionID)
	}

	// R20260603-ARCH-2 (#1681) / RNEW-003 (#423): the success-path latency
	// observability (completion log + histogram + slow-tail counter/warn) is a
	// self-contained concern with no early-return and no ctx use, so it is
	// extracted to observeSuccessLatency to shave one of executeOpt's mixed
	// concerns. Behaviour-preserving — the same three signals fire in the same
	// order against the same startedAt.
	//
	// R20260607-GO-002: compute successEndedAt once from the injectable clock
	// and share it between observeSuccessLatency and finishRun. A single
	// s.now() read keeps step-based test clocks deterministic (2 reads total:
	// startedAt here and successEndedAt below) while ensuring elapsed and
	// DurationMS both come from the same clock rather than mixing s.now() with
	// time.Since(startedAt) which reads real wall time.
	successEndedAt := s.now()
	s.observeSuccessLatency(successEndedAt.Sub(startedAt), result, snap, lg)
	// #1829: release the fresh-context session now that the run succeeded.
	// cron sessions are spawned Exempt=true (line ~1517) so the TTL cleanup
	// loop skips them entirely (router_cleanup.go: `if s.exempt { continue }`).
	// Without an explicit teardown the just-finished CLI — plus its 5~7 MCP
	// node subprocesses, ~1.6 GB resident — sits idle-but-unreclaimable until
	// the NEXT tick's preflight Reset (24h for a @daily job). Reaping here
	// instead of at next-tick-start closes that idle gap.
	//
	// Ordering: Reset MUST happen while we still hold the inflight CAS gate
	// (i.e. BEFORE finishRun → finalizer.finalize() releases it). A concurrent
	// TriggerNow that won the CAS could otherwise run its own preflight Reset +
	// GetOrCreate in the gap, and a late Reset here would tear down THAT run's
	// fresh session (run-A clobbering run-B). The per-job CAS serialisation
	// (invariant (1) in freshContextPreflightP0) guarantees no sibling cron run
	// can be mid-flight while we hold the gate, so Reset here is race-free for
	// the same reason the preflight Reset is.
	//
	// Only fresh-context jobs are reaped: persistent-mode (snap.fresh=false)
	// sessions are reused across ticks by design to carry conversational
	// context, so tearing them down would defeat the mode. They also already
	// receive normal lifecycle handling and are rare.
	//
	// re-register a suspended stub immediately so the dashboard sidebar row
	// stays visible during the idle gap, chained to result.SessionID so the
	// JSONL history of the run we just finished remains clickable. This mirrors
	// the preflight Reset → GetOrCreate(stub) shape, minus the live process.
	//
	// Job-existence re-check before re-registering the stub: DeleteJobByID's
	// teardown (deleteJobPostCleanup → resetRouterStub) does NOT take the
	// inflight CAS gate — Delete is not a cron run — so it can land
	// concurrently with this success tail. If a Delete's Reset slips between
	// our Reset and registerStubByValue, an unconditional register would
	// resurrect a sidebar stub for a job that no longer exists (zombie row).
	// This is the same orphan the preflight guards against with its post-Reset
	// stillExists check (~line 712); apply the identical guard here. Reset
	// itself is always safe (a concurrent Delete Reset is idempotent), so only
	// the register is gated.
	if snap.fresh {
		s.router.Reset(key)
		s.mu.RLock()
		_, stillExists := s.jobs[snap.jobID]
		s.mu.RUnlock()
		if stillExists {
			s.registerStubByValue(snap.jobID, snap.workDir, snap.prompt, result.SessionID)
			if result.SessionID == "" {
				// registerStubByValue chains the stub only when the session ID is
				// non-empty; an empty ID (process.go normally fills it on the
				// success frame, so empty here is anomalous) registers a chain-less
				// stub — the sidebar row survives but has no clickable JSONL
				// history. Surface it instead of silently registering a dead row.
				lg.Warn("cron fresh context: session released after successful run but session_id empty; re-registered chain-less stub with no clickable history",
					"job_id", snap.jobID)
			} else {
				lg.Info("cron fresh context: session released after successful run", "session_id", result.SessionID)
			}
		} else {
			lg.Info("cron fresh context: session released; job deleted mid-run, skipping stub re-register",
				"session_id", result.SessionID)
		}
	}
	// 把本次产生的 Claude session_id 也记下来：fresh_context=true 的
	// 路径下一次 Reset 会清掉 stub 的 chain，不保留这个 ID 的话
	// dashboard 点击 cron 侧边栏就看不到上一次的 JSONL 历史。
	// Send 路径的 result 帧总会带 SessionID（process.go 成功分支会填），
	// 传空只会出现在错误路径，finishRun 的 "" 分支自行短路。
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, endedAt: successEndedAt, trigger: trigger,
		state: RunStateSucceeded, sessionID: result.SessionID, result: result.Text,
		prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		finalizer: finalizer,
	})

	// R234-SEC-1: deliverNotice 必须用经过 sanitiseRunResult 的文本，
	// 否则未截断 / 未脱敏的 claude 输出会绕过所有保护落到 IM 渠道
	// （prompt-injection / IM 富文本指令 / 巨量响应耗尽队列）。
	// finishRun 在持久化路径已做过同样处理，这里复用相同管线。
	//
	// R20260531070014-ARCH-1: claude -p 可 exit 0 但 result.Text 为
	// API-error envelope（含 request ID / 内部 hostname / 泄漏 cred）。
	// dispatch IM 路径已通过 localizeAPIError（封装 apierr.Localize）防护；
	// cron 成功路径之前完全绕过此保护。先 sanitise（截断/脱敏）再 localize
	// （本地化/隐藏敏感 envelope），顺序以隐私优先。
	replyText := formatCronNotice(snap.labelOrID(), apierr.Localize(sanitiseRunResult(result.Text)))
	s.deliverNotice(notifyTo, replyText)
}

// observeSuccessLatency emits the three success-path latency signals for a
// completed cron run: the "cron job completed" info log, the execution-
// duration histogram, and the slow-tail counter + warn when elapsed exceeds
// the configured slowThreshold. Extracted from executeOpt to localise the
// success-only observability concern (R20260603-ARCH-2 / #1681, RNEW-003 /
// #423); the function has no ctx use and no early-return so the move is
// behaviour-preserving.
//
// Observed here (not in finishRun) because only the success path carries a
// meaningful end-to-end latency — error / timeout / canceled paths are
// classified by the CronRun*Total state counters instead, and folding their
// (often deadline-clamped) durations into the histogram would skew the
// success-latency distribution operators alert on. OBS1 (#392) / R208-OBS1.
// observeSuccessLatency receives elapsed pre-computed by the caller via
// s.now().Sub(startedAt) (R20260607-GO-002: injectable clock, consistent
// with finishRun's endedAt). Accepting elapsed directly avoids an extra
// s.now() call that would advance step-based test clocks an extra tick.
func (s *Scheduler) observeSuccessLatency(elapsed time.Duration, result SendResult, snap jobSnapshot, lg *slog.Logger) {
	lg.Info("cron job completed",
		"result_len", len(result.Text),
		"elapsed_ms", elapsed.Milliseconds())
	// OBS1 (#392): record the full success-path latency distribution, not
	// just the slow-tail count below. The histogram buckets straddle
	// slowThreshold so the two signals stay consistent (anything past 30s
	// lands in the same tail buckets the slow counter alerts on).
	metrics.ObserveCronExecutionDuration(elapsed.Milliseconds())
	slowThreshold := s.slowThreshold
	if slowThreshold <= 0 {
		slowThreshold = defaultCronSlowThreshold
	}
	if elapsed > slowThreshold {
		// R208-OBS1: poor-man's histogram — a single counter that fires
		// when a successful execution takes longer than slowThreshold.
		// R241-ARCH-11 (#519): threshold reads s.slowThreshold (config-
		// supplied) with the package default as fallback.
		metrics.CronExecutionSlowTotal.Add(1)
		lg.Warn("cron execution slow",
			"job_id", snap.jobID,
			"elapsed_ms", elapsed.Milliseconds(),
			"threshold_ms", slowThreshold.Milliseconds())
	}
}

// getSessionArgs bundles the inputs to executeGetSession (the spawn phase of
// executeOpt). A struct literal keeps the call site readable — like
// preflightArgs — and lets new spawn-phase inputs land here without re-flowing
// a long positional signature. RNEW-003 (#423).
type getSessionArgs struct {
	// ctx is the spawn-only timeout context (s.stopCtx + jobTimeout). It owns
	// the GetOrCreate call exclusively; executeGetSession cancels it via
	// spawnCancel on the success path so its *time.Timer frees before Send.
	ctx         context.Context
	spawnCancel context.CancelFunc
	// key / opts feed router.GetOrCreate. opts is the per-run cloned AgentOpts
	// (Exempt + backend/workspace overrides already applied by executeOpt).
	key  string
	opts AgentOpts
	// job / snap carry the run's identity. Failure branches route job into
	// finishRun and read snap.prompt/workDir/fresh + labelOrID for the notice.
	job  *Job
	snap jobSnapshot
	// runID / startedAt / trigger pair the synthetic finishRun with the
	// emitRunStarted frame already broadcast by executeOpt.
	runID     string
	startedAt time.Time
	trigger   TriggerKind
	// lg is the per-run logger; notifyTo is the resolved IM target for the
	// session-error notice (canceled path stays silent — shutdown races
	// should not spam IM).
	lg       *slog.Logger
	notifyTo NotifyTarget
	// finalizer is the per-run cleanup hook threaded into finishRun on the
	// failure branches; stubRefresh re-registers the sidebar row when a fresh
	// spawn aborted. inflight receives the early SessionID capture on success.
	finalizer   *runFinalizer
	stubRefresh stubRefresher
	inflight    *runInflight
}

// executeGetSession runs the spawn phase of a cron execution: GetOrCreate
// under the spawn-only ctx, classify + terminate on error, then free the
// spawn timer and capture the session_id into the inflight view on success.
//
// Return contract:
//   - abort=true  → executeGetSession already drove finishRun (+ deliverNotice
//     on the non-cancel error branch) and ran stubRefresh; the caller MUST
//     return from executeOpt immediately without touching sess.
//   - abort=false → sess is the live session and spawnStart is the pre-
//     GetOrCreate timestamp the caller uses to size the send budget warn.
//
// CTX OWNERSHIP (RNEW-003 / #423): this is the single owner of args.ctx — it
// is consumed by GetOrCreate and cancelled here on the success path (R250-GO-15
// / #1078) so its underlying *time.Timer does not idle through the Send window.
// The caller's defer spawnCancel() remains the idempotent safety net for the
// error branches (which return before reaching the success-path cancel).
func (s *Scheduler) executeGetSession(a getSessionArgs) (sess Session, spawnStart time.Time, abort bool) {
	a.inflight.setPhase(PhaseSpawning)
	// R250-CR-22 (#1155): capture spawnStart immediately before GetOrCreate
	// so the "send budget exceeds job/2" warn measures actual spawn time, not
	// (jitter + spawn). startedAt is captured pre-jitter for the dashboard
	// "running 12s" badge (true wall-clock from CAS), but the warn is
	// calibrated against jobTimeout/2 to detect when the spawn phase alone
	// consumed too much of the budget — folding jitter (default up to 30s)
	// into that measurement triggers false positives on healthy jobs whose
	// schedule landed unlucky in the jitter window.
	spawnStart = time.Now()
	sess, _, err := s.router.GetOrCreate(a.ctx, a.key, a.opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent ctx cancelled mid-flight (graceful shutdown or job
			// deletion overlapping execute). The job will either be re-run
			// on the next tick or is intentionally gone; either way an IM
			// notification would be spam and the stored LastError would
			// falsely blame the job itself.
			a.lg.Info("cron session cancelled", "err", err)
			s.finishRun(finishArgs{
				job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true, // 与 historical recordResult skip 一致
				prompt:      a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
				finalizer: a.finalizer,
			})
			a.stubRefresh.run()
			return nil, spawnStart, true
		}
		state, errClass := classifyExecError(err, ErrClassSessionError)
		if errClass == ErrClassDeadlineExceeded {
			a.lg.Info("cron session deadline exceeded", "err", err)
		} else {
			// R20260603-SEC-1: sanitise before logging to strip IP:port / paths.
			a.lg.Error("cron session error", "err", sanitiseRunErrMsg(err.Error()))
		}
		s.finishRun(finishArgs{
			job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
			state: state, errClass: errClass, errMsg: "session error: " + err.Error(),
			prompt: a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
			finalizer: a.finalizer,
		})
		s.deliverNotice(a.notifyTo, formatCronNotice(a.snap.labelOrID(), "执行跳过，请稍后重试。"))
		a.stubRefresh.run()
		return nil, spawnStart, true
	}
	// R250-GO-15 (#1078): GetOrCreate consumed ctx; nothing below references
	// it (Send uses sendCtx). Cancel now to free the underlying *time.Timer
	// instead of waiting for the function-scoped defer at executeOpt return.
	// On a 500-job-deep deployment with 5min jobTimeout this trims up to
	// ~500 idle timers off runtime timerproc during the Send window. The
	// caller's defer remains the idempotent safety net (cancel is a no-op on
	// second invocation). Placed AFTER the err-return block above so an early
	// return on session error still trips that defer.
	a.spawnCancel()

	// R242-ARCH-22 (#766): populate inflight.SessionID as soon as
	// GetOrCreate returns. Persistent-mode runs reuse a session that
	// already carries its CLI session_id (set during the original spawn's
	// init handshake), so sess.SessionID() is non-empty here. Fresh-mode
	// runs spawn a new CLI whose session_id is only stamped after the
	// init turn completes — sess.SessionID() returns "" in that window
	// and the post-Send setSessionID remains the authoritative write.
	// Without this early capture, KnownSessionIDs / IsExcluded probes during
	// the Send window miss the in-flight run on persistent-mode jobs.
	// setSessionID is idempotent and same-value writes fast-path so the
	// post-Send call is a no-op when the IDs match.
	if sid := sess.SessionID(); sid != "" {
		a.inflight.setSessionID(sid)
	}
	return sess, spawnStart, false
}

// cloneAgentOpts returns a shallow copy of opts with all reference-typed
// fields (slices / maps) defensively cloned so downstream `append` /
// in-place writes cannot mutate the entry stored in Scheduler.agents.
//
// R246-GO-17 / R228-GO-P3-8: previous code only clipped ExtraArgs.
// Today AgentOpts only carries one slice field (ExtraArgs) — plus
// strings/bool — so clipping was sufficient. This helper centralises the
// clone so any future field added to cron.AgentOpts (e.g. an Env map
// or HookConfigs slice) gets defensive copy automatically rather than
// leaking shared state into the per-run mutated copy. Keep this pure /
// allocation-light: it sits on the cron run hot path.
func cloneAgentOpts(opts AgentOpts) AgentOpts {
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
