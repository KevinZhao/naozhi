// scheduler_run.go: cron run execution path — executeOpt (CAS gate, jitter,
// snapshot, fresh-context preflight, spawn/send via the session router, send
// watchdog, terminal routing into finishRun) and its exec* helpers.

package cron

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"

	robfigcron "github.com/robfig/cron/v3"
)

// preflightArgs bundles the inputs to freshContextPreflightP0; named fields
// keep the eight inputs from silently swapping positions. Field order is size
// DESC so the value type packs without intra-struct padding.
type preflightArgs struct {
	// snap 是 snapshotJob 拷贝出的快照；preflight 优先读 snap 而非 *job，
	// 避免与并发 DeleteJob/PauseJob 起读写竞争。
	snap jobSnapshot
	// startedAt 是 caller 进入 executeOpt 时记录的 wall-clock 起点；
	// preflight 失败也保留这个起点而非重新 time.Now()，让 dashboard 看到
	// 真实的"从触发到放弃"时长。
	startedAt time.Time
	// notifyTo 是工作目录不可达分支回写中文提示的目标；其它失败分支不通知，
	// 因为「shutdown / Reset 失败」对终端用户没有可操作信号。
	notifyTo NotifyTarget
	// key 是 router GetOrCreate / Reset 用到的 session key（`cron:<jobID>`）。
	key string
	// runID 转给失败分支的 finishRun，使 cron_run_ended 与 cron_run_started 配对。
	runID string
	// trigger 区分 TriggerScheduled / TriggerManual，决定 notice 与 timeline 的渲染。
	trigger TriggerKind
	// job 只在失败分支通过 finishArgs.job 转交给 finishRun；preflight 不修改 *Job。
	job *Job
	// lg 是带 jobID/runID 标签的 slog.Logger，preflight 自身只输出
	// info/warn 不输出 error（error 由 finishRun 的 errMsg 落盘统一处理）。
	lg *slog.Logger
	// finalizer 转交给失败分支的 finishRun，让 cron_run_ended broadcast 之前
	// finalize 元数据，CurrentRun(jobID) 与 broadcast 同步可见 ok=false。
	finalizer *runFinalizer
}

// stubRefresher carries the snap-time chain anchor (jobID + workDir + prompt
// + lastSessionID) that the error-path sidebar re-registration needs. The
// four fields are the entire dependency surface, and the zero value is a safe
// no-op (run() short-circuits when active is false), so the persistent-mode /
// early-bail paths need no special-casing.
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

// freshContextPreflightP0 handles the fresh-mode prologue: ctx-cancel guard,
// work-dir reachability + containment checks, Reset, and the post-Reset
// existence re-check that prevents a CLI process orphaned on a deleted job.
// Each failure branch records its (RunState, ErrorClass) via finishRun; in
// persistent mode (snap.fresh=false) it short-circuits with ok=true.
//
// ok=false means the caller MUST return immediately: the helper has already
// logged, re-registered the sidebar stub where applicable, and called finishRun.
// That stub re-register runs BEFORE finishRun releases the CAS gate (#2318), so
// stubRefresh is the no-op zero value on failure and live only when ok=true.
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
	// Containment re-check BEFORE the destructive Reset: resolveCronWorkspace
	// used the TTL-cached view, so a symlink retargeted outside allowedRoot
	// within that TTL would otherwise reach here and blow away a live session
	// for a run that can never succeed. The uncached workDirUnderRoot fails the
	// run without tearing down the existing session.
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
	// Fresh-context atomicity (#401): Reset here and the caller's later
	// GetOrCreate are separate router acquisitions; correctness rests on
	// (1) cron↔cron serialisation by the per-jobID inflight CAS gate and
	// (2) the cron: key namespace being reserved for the scheduler. If users
	// ever send into a cron:<jobID> session, migrate to router ResetAndRecreate.
	s.router.Reset(args.key)
	lg.Info("cron fresh context: session reset before run")
	// refresh uses snap-time values (no s.jobs re-read) as the chain anchor
	// for failure paths. Two independent RLock reads of s.jobs[snap.jobID] are
	// intentional (#1298): (a) inside run(), which may fire seconds later and
	// must re-check existence; (b) right after Reset, to refuse GetOrCreate for
	// a job deleted mid-execute. Do not merge them.
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
		// Re-register the sidebar stub BEFORE the finishRun below releases the
		// inflight CAS gate (#2318): after release a concurrent TriggerNow could
		// spawn run-B's live stub and this stale one would clobber it. run()
		// re-checks existence, so for the steady-state delete it is a no-op.
		refresh.run()
		// Job deleted mid-execute: treat as canceled; no recordResult but
		// broadcast for visibility.
		s.finishRun(finishArgs{
			job: args.job, runID: args.runID, startedAt: args.startedAt, trigger: args.trigger,
			state: RunStateCanceled, errClass: ErrClassCanceled,
			errMsg: "job deleted mid-execute", skipPersist: true,
			prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: args.finalizer,
		})
		return noopRefresh, false
	}
	return refresh, true
}

// applyJitterAndRecheck performs the post-CAS jitter sleep and the post-jitter
// delete/pause recheck for a scheduled (non-TriggerNow) run with jitter enabled.
//
// When the recheck passes, snap is the under-RLock snapshot of j and snapTaken
// is true so the caller skips the redundant snapshotJob. abort=true means a
// DeleteJob / PauseJobByID landed during the jitter window; the caller MUST
// return immediately (the deferred finalizer releases the inflight CAS + gauge).
// Only invoked when !viaTriggerNow && s.jitterMax > 0, after inflight metadata
// is populated (so setPhase(PhaseJittering) is the correct transition).
func (s *Scheduler) applyJitterAndRecheck(j *Job, runID string, inflight *runInflight) (snap jobSnapshot, snapTaken bool, abort bool) {
	inflight.setPhase(PhaseJittering)
	// Snapshot Schedule / entryID / cachedPeriod under s.mu.RLock so a concurrent
	// UpdateJob cannot race the reads. The pre-parsed robfigcron.Schedule comes
	// from s.cron.Entry(entryID) instead of re-parsing the schedule string;
	// entryID==0 (not yet registered, e.g. tests) or a concurrently removed
	// entry falls back to the string-parse path.
	s.mu.RLock()
	schedStr := j.Schedule
	entryID := j.entryID
	cachedPeriod := j.cachedPeriod
	var parsedSched robfigcron.Schedule
	if entryID != 0 && cachedPeriod <= 0 {
		// Only fetch the parsed Schedule when the period cache is cold;
		// registerJob populates cachedPeriod alongside entryID.
		parsedSched = s.cron.Entry(entryID).Schedule
	}
	s.mu.RUnlock()
	switch {
	case cachedPeriod > 0:
		// hot path — period was cached at registerJob time.
		jitterSleep(s.stopCtx, cachedPeriod, s.jitterMax)
	case parsedSched != nil:
		applyJitterSched(s.stopCtx, parsedSched, s.jitterMax)
	default:
		applyJitter(s.stopCtx, schedStr, s.jitterMax)
	}

	// A DeleteJob OR PauseJobByID that lands during the (up to 30s) jitter
	// window must abort before spawn/send: the registerJob closure's paused
	// check ran BEFORE the sleep. The snapshot is taken under the SAME RLock
	// when the recheck passes so it reflects the instant the recheck verified
	// (#1351). jitter_test.go pins this recheck against silent removal.
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
// the CLI wrapper, re-validating allowedRoot containment at execute time to
// close the symlink-swap race (validateWorkspace resolved symlinks once at
// creation). With allowedRoot set the symlink-resolved path is handed to the
// CLI so the validated view matches the opened view. With allowedRoot unset a
// best-effort EvalSymlinks (not bare Clean, which does not resolve symlinks) is
// used, falling back to the cleaned raw input on error — losing resolution beats
// refusing to run when sandbox is off by operator choice (#638).
//
// abort=true (after finishRun for the outside-root class) → caller MUST return.
func (s *Scheduler) resolveCronWorkspace(
	j *Job, snap jobSnapshot, runID string, startedAt time.Time,
	trigger TriggerKind, lg *slog.Logger, finalizer *runFinalizer,
) (workDirForCLI string, abort bool) {
	if s.allowedRoot != "" {
		// Cached EvalSymlinks (TTL workDirResolveCacheTTL) so fast-firing jobs
		// don't repeat the resolve; a retarget surfaces within one TTL.
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
		// The cached gate can pass on a stale-positive within the TTL (symlink
		// retargeted outside allowedRoot after the cache warmed); re-run the
		// uncached gate so no CLI launches under the retargeted path (#1730).
		// On success keep the cached-resolved path to avoid double-EvalSymlinks
		// semantic divergence.
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

// executeOpt drives one cron run end-to-end through the exec* helper pipeline
// below. The orchestrator keeps in its own frame only what lives for the WHOLE
// run: the finalizer (CAS release + gauge), the spawn-ctx defer (safety net
// behind executeGetSession's eager cancel), and the spawn-vs-send budget split
// (two independent ctxs, never one threaded through both phases).
//
// Phase badge switch points (dashboard reads runInflight.Phase): PhaseQueued in
// execPopulateInflight, PhaseJittering in applyJitterAndRecheck, PhaseSpawning
// in executeGetSession, PhaseSending in execSend. inflight_phase_test.go pins it.
func (s *Scheduler) executeOpt(j *Job, viaTriggerNow bool) {
	// Nil-router self-defence: the inflight CAS has not been taken yet, so an
	// early return is safe; without it Reset/GetOrCreate would NPE deep in the
	// run loop with CAS gates held.
	if s == nil || s.router == nil {
		slog.Error("cron: router is nil; skipping run",
			"id", func() string {
				if j == nil {
					return ""
				}
				return j.ID
			}())
		// Synthetic started→ended pair keeps dashboard counters consistent;
		// errClass=router_missing distinguishes it from a real overlap (#1323).
		if s != nil && j != nil {
			s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassRouterMissing, "router unavailable", "router-missing")
		}
		return
	}
	// The finalizer defer (runScaffold.run wrapping the WHOLE executeAcquired
	// body) and the spawn-ctx defer (executeAcquired's frame) both fire at run
	// end — moving either into a phase helper would fire it at helper return.
	inflight, ok := s.execAcquireSlot(j, viaTriggerNow)
	if !ok {
		// CAS lost: execAcquireSlot already emitted the overlap-skip pair.
		// No finalizer exists yet and the gauge was never incremented, so a
		// bare return is the correct (and only) cleanup.
		return
	}
	// finalizer 是本次 run 的栈局部清理器：finishRun 在 emitRunEnded 之前调一次，
	// scaffold 的 defer 兜底覆盖早返路径。并发隔离来自 finalizer 的 per-run 身份
	// （done 标记），run-A 的 defer 绝不会动到 run-B 已抢占的 *runInflight (#689)。
	finalizer := &runFinalizer{inflight: inflight}
	// No onPanic: this path keeps propagating to the caller's recover boundary
	// (executeIfNotDeletedOrPaused / robfig Recover).
	runScaffold{finalizer: finalizer, jobID: j.ID}.run(func() {
		s.executeAcquired(j, viaTriggerNow, inflight, finalizer)
	})
}

// executeAcquired is the post-admission body of executeOpt: everything that
// runs once the CAS slot is held and the runScaffold envelope (finalizer defer
// + inflight gauge) is armed. Every early `return` here lands in the scaffold's
// defer, which releases the slot. Kept directly below executeOpt so the
// source-anchor tests' whole-file ordering assumptions hold.
func (s *Scheduler) executeAcquired(j *Job, viaTriggerNow bool, inflight *runInflight, finalizer *runFinalizer) {
	runID, startedAt, trigger, ok := s.execPopulateInflight(j, viaTriggerNow, inflight)
	if !ok {
		return
	}

	snap, notifyTo, lg, abortSnap := s.execSnapshotAndEmit(j, viaTriggerNow, runID, startedAt, trigger, inflight)
	if abortSnap {
		return
	}

	// Per-job timeout is always s.execTimeout: robfig/cron's SkipIfStillRunning
	// chain wrapper drops a colliding tick instead of killing a long-running job,
	// so the deadline does not need to anticipate the next tick.
	jobTimeout := s.execTimeout
	// spawnCtx 只覆盖 GetOrCreate 阶段（#1078）：executeGetSession 在
	// GetOrCreate 出口显式 cancel 以尽早释放底层 timer（否则 N 个并发 job 会在
	// 整个 Send 阶段占着 timer 槽位）；defer 仍兜底（cancel 幂等）。
	ctx, spawnCancel := context.WithTimeout(s.stopCtx, jobTimeout)
	defer spawnCancel()

	// execPrepareSpawn also owns the sandbox placement fork: a sandbox job
	// self-terminates via finishRun inside the helper and returns
	// okSpawnPrep=false, so the same return covers it. inflight + spawnCancel
	// let the helper release the spawn timer before the long sandbox invoke.
	opts, key, cleanText, stubRefresh, okSpawnPrep := s.execPrepareSpawn(j, snap, runID, startedAt, trigger, lg, notifyTo, finalizer, inflight, spawnCancel)
	if !okSpawnPrep {
		return
	}

	sess, spawnStart, abortSpawn := s.executeGetSession(getSessionArgs{
		ctx: ctx, spawnCancel: spawnCancel, key: key, opts: opts,
		job: j, snap: snap, runID: runID, startedAt: startedAt, trigger: trigger,
		lg: lg, notifyTo: notifyTo, finalizer: finalizer,
		stubRefresh: stubRefresh, inflight: inflight,
	})
	if abortSpawn {
		return
	}

	// Send is parented on s.stopCtx so Stop() can short-circuit an in-flight
	// Send (#790, #500). sendBudget = jobTimeout minus spawn time, floored at
	// minSendBudget so flaky cold-start spawns don't immediately surface as
	// "send timed out" while bounding wall-clock to jobTimeout+minSendBudget
	// (#1311). spawnElapsed is captured once so budget and warn log agree.
	spawnElapsed := time.Since(spawnStart)
	sendBudget := jobTimeout - spawnElapsed
	if sendBudget < minSendBudget {
		sendBudget = minSendBudget
	}

	result, ok := s.execSend(execSendArgs{
		job: j, sess: sess, snap: snap, cleanText: cleanText,
		sendBudget: sendBudget, spawnElapsed: spawnElapsed, jobTimeout: jobTimeout,
		key: key, runID: runID, startedAt: startedAt, trigger: trigger,
		lg: lg, notifyTo: notifyTo, finalizer: finalizer,
		stubRefresh: stubRefresh, inflight: inflight,
	})
	if !ok {
		return
	}

	s.execFinishSuccess(j, snap, key, result, runID, startedAt, trigger, lg, notifyTo, finalizer)
}

// execAcquireSlot is the CAS admission gate of a cron run.
//
// The jobInflight load and the CAS MUST be one atomic step relative to
// cleanupRunningJobIfIdle's Load→CompareAndDelete: holding the per-jobID gate
// across both closes the window where a DeleteJob racing TriggerNow deletes the
// map entry between our load and CAS, leaving us CASing an orphaned gate while a
// second executeOpt LoadOrStores a fresh one (#1706). The gate is released right
// after the CAS; the heavy run body does not need it.
//
// ok=false → the overlap skip pair was emitted; caller returns without cleanup.
func (s *Scheduler) execAcquireSlot(j *Job, viaTriggerNow bool) (inflight *runInflight, ok bool) {
	// TriggerNow bypasses the cron chain's SkipIfStillRunning, so the per-job
	// *runInflight CAS is the uniform overlap guard for both paths.
	gate := s.jobGateLock(j.ID)
	gate.Lock()
	inflight = s.jobInflight(j.ID)
	won := inflight.running.CompareAndSwap(false, true)
	gate.Unlock()
	if !won {
		slog.Info("cron: job already running, skipping overlap", "job_id", j.ID)
		// Overlap is a skipped state (no LastRunAt update). Counters /
		// broadcast still fire so dashboards can surface the skip.
		s.emitOverlapSkipped(j, viaTriggerNow)
		return nil, false
	}
	return inflight, true
}

// execPopulateInflight runs the post-CAS preconditions and publishes the run's
// inflight metadata.
//
// ok=false → a synthetic skip pair was emitted (or generateRunID failed with a
// plain log) and the caller must return; the caller's finalizer defer handles
// the CAS release + gauge decrement, so no cleanup happens here.
func (s *Scheduler) execPopulateInflight(j *Job, viaTriggerNow bool, inflight *runInflight) (runID string, startedAt time.Time, trigger TriggerKind, ok bool) {
	// Post-CAS paused/deleted recheck (#1322): the dispatch callers check under
	// s.mu.RLock and release it BEFORE executeOpt, leaving a µs window where
	// Pause/Delete can land; the jitter-window recheck does not cover TriggerNow
	// or jitter==0 ticks. Recheck once here before any heavy work; the caller's
	// finalizer defer releases the CAS on this early return.
	s.mu.RLock()
	curCAS, stillRegisteredCAS := s.jobs[j.ID]
	pausedCAS := stillRegisteredCAS && curCAS.Paused
	s.mu.RUnlock()
	if !stillRegisteredCAS || pausedCAS {
		casLg := slog.With("job_id", j.ID, "trigger_now", viaTriggerNow)
		if !stillRegisteredCAS {
			casLg.Debug("cron: job deleted between dispatch lookup and CAS, aborting run")
			// Synthetic started→ended pair so subscribers see a complete
			// lifecycle frame instead of a gap (#1410).
			s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassDeletedConcurrent, "job deleted between dispatch and CAS", "deleted-during-dispatch")
		} else {
			casLg.Debug("cron: job paused between dispatch lookup and CAS, aborting run")
			// Pause in the cross-lock window also gets a synthetic pair (#1410).
			s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassPausedConcurrent, "job paused between dispatch and CAS", "paused-during-dispatch")
		}
		return "", time.Time{}, "", false
	}

	// Populate the inflight metadata under the CAS-true window. RunID is
	// generated once per run; StartedAt is captured before jitter so the
	// "running 12s" badge in the UI counts true wall-clock from CAS.
	runID, err := generateRunID()
	if err != nil {
		// crypto/rand 不可用时不能 panic：log + skip 该次 tick，下一周期自然恢复
		// （getrandom 失效是瞬时的内核事件）。caller 的 defer 已覆盖 inflight 释放。
		slog.Error("cron: failed to generate run ID; skipping tick",
			"job_id", j.ID, "trigger_now", viaTriggerNow, "err", err)
		return "", time.Time{}, "", false
	}
	// Read via the injected clock so a fake clock can pin a deterministic run
	// duration end-to-end (endedAt in finishRun also flows through s.now()).
	startedAt = s.now()
	trigger = TriggerScheduled
	if viaTriggerNow {
		trigger = TriggerManual
	}
	inflight.populate(runInflightView{
		RunID:     runID,
		StartedAt: startedAt,
		Phase:     PhaseQueued,
		Trigger:   trigger,
	})
	// CronRunStartedTotal bumps inside emitRunStarted.
	return runID, startedAt, trigger, true
}

// execSnapshotAndEmit applies jitter, snapshots the job, resolves the notify
// target, broadcasts RunStarted, and builds the per-run logger.
//
// abort=true → the jitter-window recheck observed a delete/pause and already
// drove its own finish path inside applyJitterAndRecheck; the caller returns
// and its finalizer defer releases the slot.
func (s *Scheduler) execSnapshotAndEmit(j *Job, viaTriggerNow bool, runID string, startedAt time.Time, trigger TriggerKind, inflight *runInflight) (snap jobSnapshot, notifyTo NotifyTarget, lg *slog.Logger, abort bool) {
	// snapTaken tracks whether the jitter block already took the snapshot under
	// the recheck's RLock; snapshotting twice could read a fresher UpdateJob than
	// the recheck observed, breaking the "snapshot reflects the verified instant"
	// contract.
	var snapTaken bool

	// Jitter after CAS (so overlap triggers are rejected immediately) and before
	// snapshot (so an UpdateJob during the window still takes effect this run).
	// TriggerNow skips jitter to preserve "run now = run now".
	if !viaTriggerNow && s.jitterMax > 0 {
		var abortJitter bool
		snap, snapTaken, abortJitter = s.applyJitterAndRecheck(j, runID, inflight)
		if abortJitter {
			return jobSnapshot{}, NotifyTarget{}, nil, true
		}
	}

	// Snapshot mutable Job fields once under s.mu so the rest of the run is
	// lock-free; concurrent SetJobPrompt/UpdateJob land for the next tick.
	if !snapTaken {
		snap = s.snapshotJob(j)
	}
	inflight.setFresh(snap.fresh)

	// Resolve the effective notification target. Returns empty struct
	// when no delivery should happen, so both success and failure paths
	// can call notify*() unconditionally-guarded by IsSet().
	notifyTo = s.resolveNotifyTarget(snap.platName, snap.chatID, snap.notifyPlat, snap.notifyChat, snap.notify)

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

	// One slog.With per execution is deliberate: the chain is reused 20+ times
	// downstream, and caching it on *Job would need invalidation on every
	// platform/chat mutation — not worth ~200ns per tick (#666, won't-fix).
	lg = slog.With("job_id", snap.jobID, "platform", snap.platName, "chat", osutil.SanitizeForLog(snap.chatID, 64), "run_id", runID) // chatID is attacker-influenced; strip C1/bidi log-injection chars
	lg.Info("cron job executing", "prompt_len", len(snap.prompt))
	return snap, notifyTo, lg, false
}

// execPrepareSpawn resolves the agent/backend/workspace options and runs the
// fresh-context preflight for a run.
//
// ok=false → resolveCronWorkspace, freshContextPreflightP0, or the sandbox
// placement fork already drove finishRun (and ran stubRefresh where
// applicable); the caller must return.
//
// inflight + spawnCancel are threaded in for the sandbox fork only: a sandbox
// job releases the spawn-phase timer up front and hands the in-flight handle to
// the run-once microVM path, which never touches the session router.
func (s *Scheduler) execPrepareSpawn(j *Job, snap jobSnapshot, runID string, startedAt time.Time, trigger TriggerKind, lg *slog.Logger, notifyTo NotifyTarget, finalizer *runFinalizer, inflight *runInflight, spawnCancel context.CancelFunc) (opts AgentOpts, key, cleanText string, stubRefresh stubRefresher, ok bool) {
	// agentCommands/agents are published once at construction and read
	// lock-free via configMaps(); a future hot-reload Store()s a fresh
	// *cronConfigMaps. Load once so both reads see the same generation.
	cm := s.configMaps()
	agentID, cleanText := resolveAgent(snap.prompt, cm.agentCommands)
	opts = cloneAgentOpts(cm.agents[agentID])
	opts.Exempt = true // cron sessions must not count toward maxProcs or evict user sessions
	// Per-job backend override: empty leaves the agent/router default; a
	// non-empty value wins because the user picked it explicitly. validateBackend
	// at the router boundary still rejects shape-invalid input.
	if snap.backend != "" {
		opts.Backend = snap.backend
	}

	// Placement fork (agentcore-cloud-sandbox RFC §4.2): sandbox jobs are
	// run-once microVM invocations that never touch the session router. Branch
	// here, after agent resolution and before any router state, so the local
	// path stays byte-identical; executeSandbox routes every terminal through
	// finishRun and the caller's finalizer still releases the CAS gate.
	if placementIsSandbox(snap.placement) {
		// Release the spawn-phase timer now: the sandbox path derives its own
		// budget, and keeping this ctx alive would hand later code a misleading one.
		spawnCancel()
		s.executeSandbox(sandboxExecArgs{
			job: j, snap: snap, runID: runID, startedAt: startedAt,
			trigger: trigger, prompt: cleanText, model: opts.Model,
			notifyTo: notifyTo, inflight: inflight, finalizer: finalizer,
			lg: lg,
		})
		return AgentOpts{}, "", "", stubRefresher{}, false
	}

	if snap.workDir != "" {
		workDirForCLI, abort := s.resolveCronWorkspace(j, snap, runID, startedAt, trigger, lg, finalizer)
		if abort {
			return AgentOpts{}, "", "", stubRefresher{}, false
		}
		opts.Workspace = workDirForCLI
	}
	key = sessionkey.CronKey(snap.jobID)

	// Fresh mode: drop any existing session (process + history) so GetOrCreate
	// spawns a brand-new CLI. On error paths the returned stubRefresh
	// re-registers the sidebar row; on success the live session carries its own.
	stubRefresh, okPreflight := s.freshContextPreflightP0(preflightArgs{
		job: j, snap: snap, key: key, lg: lg, notifyTo: notifyTo,
		runID: runID, startedAt: startedAt, trigger: trigger,
		finalizer: finalizer,
	})
	if !okPreflight {
		// Do NOT call stubRefresh.run() here: the preflight already re-registered
		// the stub BEFORE its own finishRun released the CAS gate (#2318).
		return AgentOpts{}, "", "", stubRefresher{}, false
	}
	return opts, key, cleanText, stubRefresh, true
}

// execSendArgs bundles the inputs to execSend (the send phase of executeOpt);
// mirrors getSessionArgs.
type execSendArgs struct {
	job  *Job
	sess Session
	snap jobSnapshot
	// cleanText is the prompt with the agent-command prefix stripped.
	cleanText string
	// sendBudget is the remaining jobTimeout after spawn, floored at
	// minSendBudget by the caller; spawnElapsed / jobTimeout feed the warn only.
	sendBudget   time.Duration
	spawnElapsed time.Duration
	jobTimeout   time.Duration
	// key is the cron session key; fresh error/cancel branches Reset it while
	// the CAS gate is still held (#1956).
	key       string
	runID     string
	startedAt time.Time
	trigger   TriggerKind
	lg        *slog.Logger
	notifyTo  NotifyTarget
	finalizer *runFinalizer
	// stubRefresh re-registers the sidebar row on failure paths; inflight
	// receives the PhaseSending switch and the SessionID capture.
	stubRefresh stubRefresher
	inflight    *runInflight
}

// execSend runs the send phase of a cron execution: create the send-budget
// ctx, switch the inflight badge to PhaseSending, drive sendWithWatchdog, and
// terminate the run on error.
//
// ok=false → execSend already drove finishRun (+ deliverNotice on the non-cancel
// error branch) and ran stubRefresh; the caller MUST return without touching
// result. ok=true → result is the successful SendResult.
//
// CTX OWNERSHIP: sendCtx is created here, independent of the spawn ctx, and never
// escapes; sendWithWatchdog cancels it at Send completion, the defer is the safety net.
func (s *Scheduler) execSend(a execSendArgs) (result SendResult, ok bool) {
	sendCtx, sendCancel := context.WithTimeout(s.stopCtx, a.sendBudget)
	defer sendCancel()
	// Structured signal when spawn already consumed >spawnElapsedWarnRatio of
	// jobTimeout: the wall-clock doubling is intentional but operators of 300s+
	// jobs need an event to alert on (counter + slog pair).
	spawnWarnBudget := time.Duration(float64(a.jobTimeout) * spawnElapsedWarnRatio)
	// spawnElapsed excludes jitter (captured right after executeGetSession).
	if a.spawnElapsed > spawnWarnBudget {
		metrics.CronSendBudgetDoubledTotal.Add(1)
		// Message string preserved for runbook grep — see docs/ops/pprof.md
		// + internal/metrics/metrics.go CronSendBudgetDoubledTotal godoc.
		a.lg.Warn("cron send budget exceeds job/2",
			"job_id", a.snap.jobID,
			"spawn_elapsed_ms", a.spawnElapsed.Milliseconds(),
			"job_timeout_ms", a.jobTimeout.Milliseconds(),
			// send_budget is the post-clamp budget (jobTimeout - spawnElapsed, floored).
			"send_budget_ms", a.sendBudget.Milliseconds(),
			"warn_ratio", spawnElapsedWarnRatio)
	}
	a.inflight.setPhase(PhaseSending)

	// sendWithWatchdog localises the watchdog ↔ Send ordering contract (drain
	// abortCh AFTER cancelling sendCtx) so a refactor here cannot let the next
	// Reset race the in-flight interrupt write; see its godoc.
	result, abort, err := s.sendWithWatchdog(sendCtx, sendCancel, a.sess, a.cleanText)
	if err != nil {
		s.execSendError(a, abort, err)
		return SendResult{}, false
	}
	if result.SessionID != "" {
		a.inflight.setSessionID(result.SessionID)
	}
	return result, true
}

// execSendError terminates a run whose Send failed: classify, log, reap the
// fresh session while the CAS gate is held, finishRun, notify, and refresh
// the sidebar stub.
func (s *Scheduler) execSendError(a execSendArgs, abort abortResult, err error) {
	j, snap, key := a.job, a.snap, a.key
	runID, startedAt, trigger := a.runID, a.startedAt, a.trigger
	lg, notifyTo, finalizer, stubRefresh := a.lg, a.notifyTo, a.finalizer, a.stubRefresh
	if errors.Is(err, context.Canceled) {
		// Suppress the operator-facing notice so shutdown races don't look like
		// real failures. As on the deadline path, a watchdog that fired without
		// the interrupt landing is surfaced at Warn: the in-flight turn may still
		// be wedged at session level even though the run is recorded cancelled (#555).
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
		// Reset must run while the inflight CAS gate is still held, i.e. BEFORE
		// finishRun releases it (#1956): a late Reset would let a
		// concurrent TriggerNow win the CAS and then blindly delete run-B's fresh
		// session (resetLocked has no owner check).
		if snap.fresh {
			s.router.Reset(key)
		}
		// Likewise re-register the sidebar stub BEFORE finishRun releases the
		// gate; a late refresh could clobber run-B's live stub with run-A's
		// stale chain (phantom sidebar pointing at the prior session's JSONL).
		stubRefresh.run()
		s.finishRun(finishArgs{
			job: j, runID: runID, startedAt: startedAt, trigger: trigger,
			state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
			skipPersist: true,
			prompt:      snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
			finalizer: finalizer,
		})
		return
	}
	state, errClass := classifyExecError(err, ErrClassSendError)
	if errClass == ErrClassDeadlineExceeded {
		// Log alongside the watchdog outcome. A fired watchdog whose interrupt
		// did not land is surfaced at Warn — the turn may still be burning budget
		// and operators need a transport-breakage signal (#555).
		// InterruptUnsupported is excluded: ACP backends always report it.
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
		// sanitise before logging to strip IP:port / paths.
		lg.Error("cron send error", "err", sanitiseRunErrMsg(err.Error()))
	}
	// Reset must run while the inflight CAS gate is still held, i.e. BEFORE
	// finishRun releases it (#1956): a late Reset would let a
	// concurrent TriggerNow win the CAS and then blindly delete run-B's fresh
	// session (resetLocked has no owner check).
	if snap.fresh {
		s.router.Reset(key)
	}
	// Stub re-register BEFORE finishRun releases the gate (see the cancel
	// branch); deliverNotice (IM, stub-independent) stays after finishRun.
	stubRefresh.run()
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: trigger,
		state: state, errClass: errClass, errMsg: "send error: " + sanitiseRunErrMsg(err.Error()), // strip IP:port/paths, mirrors lg.Error above
		prompt: snap.prompt, workDir: snap.workDir, fresh: snap.fresh,
		finalizer: finalizer,
	})
	s.deliverNotice(notifyTo, formatCronNotice(snap.labelOrID(), "执行失败，请稍后重试。"))
}

// execFinishSuccess records a successful run: latency observability, the
// fresh-session reap (while the CAS gate is held), the success finishRun, and
// the sanitised IM notice.
func (s *Scheduler) execFinishSuccess(j *Job, snap jobSnapshot, key string, result SendResult, runID string, startedAt time.Time, trigger TriggerKind, lg *slog.Logger, notifyTo NotifyTarget, finalizer *runFinalizer) {
	// successEndedAt is read once from the injectable clock and shared by
	// observeSuccessLatency and finishRun so elapsed and DurationMS come from
	// the same reading (step-based test clocks stay deterministic).
	successEndedAt := s.now()
	s.observeSuccessLatency(successEndedAt.Sub(startedAt), result, snap, lg)
	// Release the fresh-context session now that the run succeeded (#1829):
	// cron sessions are Exempt from TTL cleanup, so without this the finished
	// CLI (+ MCP subprocesses, ~1.6 GB) would idle until the next tick's Reset.
	// Persistent-mode sessions are reused across ticks by design. The reap MUST
	// precede finishRun (CAS release) — see reapFreshSessionLocked (#1911).
	if snap.fresh {
		s.reapFreshSessionLocked(key, snap, result.SessionID, lg)
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
		// persist the local-run cost so per-job monthly aggregates are not 0 (#2280).
		costUSD:   result.CostUSD,
		finalizer: finalizer,
	})

	// deliverNotice 必须用经过 sanitise 的文本，否则未截断/未脱敏的 claude 输出会
	// 绕过所有保护落到 IM 渠道（prompt-injection / 巨量响应）。claude -p 可 exit 0
	// 但 result.Text 为 API-error envelope（含 request ID / 内部 hostname），故先
	// sanitise 再 localize，顺序以隐私优先。
	replyText := formatCronNotice(snap.labelOrID(), localizeNotice(result.Text))
	s.deliverNotice(notifyTo, replyText)
}

// reapFreshSessionLocked tears down the just-finished fresh-context session
// (Reset) and re-registers a suspended sidebar stub (#1829).
//
// ORDERING CONTRACT — DO NOT REORDER: call this while the caller still holds
// the inflight CAS gate, i.e. BEFORE finishRun → finalizer.finalize() releases
// it; otherwise a concurrent TriggerNow could win the CAS and a late Reset here
// would tear down THAT run's fresh session (#1911; pinned by a source-anchor test).
//
// The stub re-register is gated on job existence: DeleteJobByID does not take
// the CAS gate, so an unconditional register could resurrect a zombie sidebar row.
func (s *Scheduler) reapFreshSessionLocked(key string, snap jobSnapshot, sessionID string, lg *slog.Logger) {
	s.router.Reset(key)
	s.mu.RLock()
	_, stillExists := s.jobs[snap.jobID]
	s.mu.RUnlock()
	if stillExists {
		s.registerStubByValue(snap.jobID, snap.workDir, snap.prompt, sessionID)
		if sessionID == "" {
			// registerStubByValue chains the stub only when the session ID is
			// non-empty; an empty ID (process.go normally fills it on the
			// success frame, so empty here is anomalous) registers a chain-less
			// stub — the sidebar row survives but has no clickable JSONL
			// history. Surface it instead of silently registering a dead row.
			lg.Warn("cron fresh context: session released after successful run but session_id empty; re-registered chain-less stub with no clickable history",
				"job_id", snap.jobID)
		} else {
			lg.Info("cron fresh context: session released after successful run", "session_id", sessionID)
		}
	} else {
		lg.Info("cron fresh context: session released; job deleted mid-run, skipping stub re-register",
			"session_id", sessionID)
	}
}

// observeSuccessLatency emits the three success-path latency signals for a
// completed cron run: the "cron job completed" info log, the execution-duration
// histogram, and the slow-tail counter + warn when elapsed exceeds slowThreshold.
//
// Observed here (not in finishRun) because only the success path carries a
// meaningful end-to-end latency — error / timeout / canceled paths are counted
// by the CronRun*Total state counters, and folding their (often
// deadline-clamped) durations into the histogram would skew the distribution
// operators alert on (#392). elapsed is passed in so the caller's single
// s.now() read is shared with finishRun (no extra clock tick in tests).
func (s *Scheduler) observeSuccessLatency(elapsed time.Duration, result SendResult, snap jobSnapshot, lg *slog.Logger) {
	lg.Info("cron job completed",
		"result_len", len(result.Text),
		"elapsed_ms", elapsed.Milliseconds())
	// Histogram buckets straddle slowThreshold so the histogram and the slow
	// counter below stay consistent (#392).
	metrics.ObserveCronExecutionDuration(elapsed.Milliseconds())
	slowThreshold := s.slowThreshold
	if slowThreshold <= 0 {
		slowThreshold = defaultCronSlowThreshold
	}
	if elapsed > slowThreshold {
		metrics.CronExecutionSlowTotal.Add(1)
		lg.Warn("cron execution slow",
			"job_id", snap.jobID,
			"elapsed_ms", elapsed.Milliseconds(),
			"threshold_ms", slowThreshold.Milliseconds())
	}
}

// getSessionArgs bundles the inputs to executeGetSession (the spawn phase of
// executeOpt); mirrors preflightArgs.
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
// abort=true → finishRun (+ deliverNotice on the non-cancel branch) and
// stubRefresh already ran; the caller MUST return without touching sess.
// abort=false → sess is live and spawnStart is the pre-GetOrCreate timestamp
// the caller uses to size the send budget. This is the single owner of
// args.ctx: GetOrCreate consumes it and the success path cancels it so its
// timer does not idle through Send; the caller's defer is the safety net.
func (s *Scheduler) executeGetSession(a getSessionArgs) (sess Session, spawnStart time.Time, abort bool) {
	a.inflight.setPhase(PhaseSpawning)
	// Capture spawnStart immediately before GetOrCreate so the "send budget
	// exceeds job/2" warn measures spawn time alone — folding jitter (up to
	// 30s) in would false-positive on healthy jobs (#1155). s.now() lets tests
	// inject a fake clock.
	spawnStart = s.now()
	sess, _, err := s.router.GetOrCreate(a.ctx, a.key, a.opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent ctx cancelled (shutdown or job deletion): an IM notice would
			// be spam and a stored LastError would falsely blame the job.
			a.lg.Info("cron session cancelled", "err", err)
			// Reset must run while the inflight CAS gate is still held, i.e. BEFORE
			// finishRun releases it (#1956): a late Reset would let a
			// concurrent TriggerNow win the CAS and then blindly delete run-B's fresh
			// session (resetLocked has no owner check).
			if a.snap.fresh {
				s.router.Reset(a.key)
			}
			// Stub re-register BEFORE finishRun releases the gate — see
			// execSendError for the rationale.
			a.stubRefresh.run()
			s.finishRun(finishArgs{
				job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
				state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: err.Error(),
				skipPersist: true, // cancel never touches LastRunAt
				prompt:      a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
				finalizer: a.finalizer,
			})
			return nil, spawnStart, true
		}
		state, errClass := classifyExecError(err, ErrClassSessionError)
		if errClass == ErrClassDeadlineExceeded {
			a.lg.Info("cron session deadline exceeded", "err", err)
		} else {
			// sanitise before logging to strip IP:port / paths.
			a.lg.Error("cron session error", "err", sanitiseRunErrMsg(err.Error()))
		}
		// Reset must run while the inflight CAS gate is still held, i.e. BEFORE
		// finishRun releases it (#1956): a late Reset would let a
		// concurrent TriggerNow win the CAS and then blindly delete run-B's fresh
		// session (resetLocked has no owner check).
		if a.snap.fresh {
			s.router.Reset(a.key)
		}
		// Stub re-register BEFORE finishRun releases the gate — see execSendError;
		// deliverNotice (IM, stub-independent) stays after finishRun.
		a.stubRefresh.run()
		s.finishRun(finishArgs{
			job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
			state: state, errClass: errClass, errMsg: "session error: " + sanitiseRunErrMsg(err.Error()), // mirrors send-error path
			prompt: a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
			finalizer: a.finalizer,
		})
		s.deliverNotice(a.notifyTo, formatCronNotice(a.snap.labelOrID(), "执行跳过，请稍后重试。"))
		return nil, spawnStart, true
	}
	// GetOrCreate consumed ctx and nothing below references it (Send uses
	// sendCtx): cancel now to free the underlying timer instead of waiting for
	// the caller's defer (up to ~500 idle timers during Send on a big deployment).
	a.spawnCancel()

	// Capture inflight.SessionID as soon as GetOrCreate returns: persistent-mode
	// sessions already carry their CLI session_id, so KnownSessionIDs probes
	// during the Send window would otherwise miss the in-flight run (#766).
	// Fresh-mode returns "" here; the post-Send setSessionID stays authoritative.
	if sid := sess.SessionID(); sid != "" {
		a.inflight.setSessionID(sid)
	}
	return sess, spawnStart, false
}

// cloneAgentOpts returns a shallow copy of opts with all reference-typed
// fields (slices / maps) defensively cloned so downstream `append` /
// in-place writes cannot mutate the entry stored in Scheduler.agents. Any
// future reference field added to AgentOpts must be cloned here. Keep this
// pure / allocation-light: it sits on the cron run hot path.
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
