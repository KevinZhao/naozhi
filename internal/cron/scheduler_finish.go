// scheduler_finish.go: terminal hooks for every cron execution path
// (write side) plus run-history queries the dashboard reads (read side).
// Keeping readers and writers together means a CronRun schema change moves
// both at once. Methods stay on *Scheduler so the s.mu / s.jobs / s.runStore /
// s.runningJobs fields remain accessible without exporting.

package cron

import (
	"context"
	"github.com/naozhi/naozhi/internal/costledger"
	"io/fs"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// RunHistoryReader is the read-only slice of *Scheduler that dashboard
// handlers concerned solely with cron run history (transcript / detail /
// list endpoints) need, so a history-read endpoint is not coupled to the
// router / platforms / execute path carried by *Scheduler (#1172). The
// underlying runStore stays unexported.
type RunHistoryReader interface {
	// CurrentRun returns the inflight snapshot for jobID, or (zero, false)
	// when the job is not currently executing.
	CurrentRun(jobID string) (RunInflightView, bool)
	// ListRuns returns up to limit CronRunSummary entries for jobID, newest
	// first, with an optional StartedAt < before cutoff.
	ListRuns(jobID string, limit int, before time.Time) []CronRunSummary
	// RecentRuns returns the newest n CronRunSummary entries for jobID.
	RecentRuns(jobID string, n int) []CronRunSummary
	// Run returns the full CronRun for runID under jobID.
	Run(jobID, runID string) (*CronRun, error)
}

// Compile-time proof that *Scheduler satisfies RunHistoryReader.
var _ RunHistoryReader = (*Scheduler)(nil)

// CurrentRun returns the inflight snapshot for jobID, or (zero, false) when
// the job is not currently executing. Used by the dashboard list API to
// show "running 12s" badges.
func (s *Scheduler) CurrentRun(jobID string) (RunInflightView, bool) {
	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return runInflightView{}, false
	}
	// Defensive: the type-erased Load would otherwise panic if a refactor
	// stores a different type or a nil value; degrade to "no inflight".
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		return runInflightView{}, false
	}
	return inf.snapshot()
}

// ListRuns returns up to limit CronRunSummary entries for jobID, newest
// first. before is a cutoff (only runs with StartedAt < before); zero
// means "no cutoff" (latest page). Returns nil when persistence is disabled
// (StorePath empty).
func (s *Scheduler) ListRuns(jobID string, limit int, before time.Time) []CronRunSummary {
	if !s.runStoreEnabled() {
		return nil
	}
	return s.runStore.List(jobID, limit, before)
}

// RecentRuns is the convenience wrapper for the cron list view's
// recent_runs field. Cap is enforced inside ListRuns.
func (s *Scheduler) RecentRuns(jobID string, n int) []CronRunSummary {
	if !s.runStoreEnabled() {
		return nil
	}
	return s.runStore.Recent(jobID, n)
}

// Run returns the full CronRun for runID under jobID. Returns
// (nil, fs.ErrNotExist) when missing; (nil, ErrCorruptRun) when present
// but unusable. Server layer maps these to 404 / 500 respectively.
func (s *Scheduler) Run(jobID, runID string) (*CronRun, error) {
	if !s.runStoreEnabled() {
		return nil, fs.ErrNotExist
	}
	return s.runStore.Get(jobID, runID)
}

// --- runStore write / lifecycle facade (#509) ---
//
// Every package-internal access to the runStore goes through a *Scheduler
// method in this file, so the storage type's surface is reachable from exactly
// one file. Each wrapper applies the nil/enabled guard and forwards verbatim,
// leaving the runStore's own lock discipline (s.mu > jobLock > entry.mu)
// untouched. TestNoDirectRunStoreAccess pins the invariant.

// runStoreEnabled reports whether run-history persistence is live: a non-nil
// Scheduler whose runStore is enabled (StorePath set). runStore.enabled()
// tolerates a nil receiver, so this never panics on a partially-constructed
// Scheduler.
func (s *Scheduler) runStoreEnabled() bool {
	return s != nil && s.runStore.enabled()
}

// jobStillExists reports whether jobID is still present in s.jobs under a
// short s.mu read lock. finishRun uses it to re-check job existence between
// recordTerminalResult (which released s.mu) and the runs/<jobID>/ disk write,
// so a concurrent DeleteJobByID does not get its runs subtree resurrected by
// appendRun's ensureJobDir (#2058).
func (s *Scheduler) jobStillExists(jobID string) bool {
	s.mu.RLock()
	_, exists := s.jobs[jobID]
	s.mu.RUnlock()
	return exists
}

// appendRun persists one CronRun via the runStore. No-op when persistence is
// disabled. Append owns its per-job jobLock internally, so this wrapper holds
// no scheduler lock.
func (s *Scheduler) appendRun(run *CronRun) {
	if !s.runStoreEnabled() {
		return
	}
	s.runStore.Append(run)
}

// recentSessionIDs returns up to n distinct non-empty SessionID strings from
// jobID's newest-first run history; nil when persistence is disabled. Reads
// off the cache ring under entry.mu — no scheduler lock involved.
func (s *Scheduler) recentSessionIDs(jobID string, n int) []string {
	if !s.runStoreEnabled() {
		return nil
	}
	return s.runStore.RecentSessionIDs(jobID, n)
}

// trimAllRuns runs the retention GC pass across every job's runs/ subtree.
// No-op when persistence is disabled. runStore.trimAllCtx takes each per-job
// jobLock internally and honours ctx cancellation at job boundaries.
func (s *Scheduler) trimAllRuns(ctx context.Context, now time.Time) {
	if !s.runStoreEnabled() {
		return
	}
	s.runStore.trimAllCtx(ctx, now)
}

// deleteJobRuns removes jobID's entire runs/ subtree and reclaims its jobLock.
// No-op when persistence is disabled. Called from deleteJobPostCleanup outside
// s.mu; runStore.DeleteJob acquires the per-job jobLock internally.
func (s *Scheduler) deleteJobRuns(jobID string) {
	if !s.runStoreEnabled() {
		return
	}
	s.runStore.DeleteJob(jobID)
	// agentcore §5.1: also drop this job's input-snapshot manifests. Blobs are
	// content-addressed and shared across jobs, so they are NOT removed here.
	// TODO(agentcore §5.2): blob GC pass (refcount or mark-sweep against live
	// manifests) for the runsnapshots/blobs/ tree.
	s.deleteJobSnapshots(jobID)
	// Drop sandbox event logs written by sandboxEventSink (§6.1); a 60-minute
	// sandbox run can accumulate several MB.
	s.deleteJobSandboxEvents(jobID)
	// §7.4: drop unresolved confirmation-queue records — a deleted job has
	// nothing left to confirm or replay.
	s.deleteJobAttention(jobID)
}

// finishArgs bundles the parameters of finishRun so each call site reads as a
// struct literal — many fields are optional and a positional signature would
// be brittle. Snapshot fields (prompt/workDir/fresh) are zero on paths that
// never took snapshotJob() (overlapSkipped / pre-snapshot preflight failures);
// the dashboard falls back to Job.Prompt for display.
type finishArgs struct {
	// job 是终结的目标 Job。state==Skipped 的 overlap 路径仍要传 *Job，
	// 因为 emitRunEnded 需要 Job.ID 作为事件 key；DeleteJob 中途的竞态由
	// recordTerminalResult 内 jobs[id] 二次校验。
	job *Job
	// runID / startedAt 与上游 emitRunStarted 的 RunStartedEvent 一一对应；
	// 订阅方 (dashboard hub) 用 RunID 配对 started→ended 帧。
	runID     string
	startedAt time.Time
	// trigger 与 RunStartedEvent.Trigger 必须一致。
	trigger TriggerKind
	// state 决定 metrics 计数桶；Skipped 不计入 Failed。
	state RunState
	// sessionID 是 GetOrCreate 分配的 CLI session_id（fresh=true 路径必为
	// 空字符串）。空值 dashboard 隐藏「打开会话」按钮。
	sessionID string
	// result 是 CLI 末轮文本输出（已经 sanitiseRunResult：4K rune 截断 +
	// …[truncated] 后缀 + SanitizeForLog 控制字符过滤）。
	result string
	// errClass 是机器可读的错误分类；dashboard 用它选图标 + i18n 文案，
	// errMsg 仅作展开详情。
	errClass ErrorClass
	// errMsg 是人类可读错误（控制符已 escape，绝对路径已 redact），
	// ≤ maxCronErrMsgRunes。
	errMsg string
	// skipPersist 同时跳过 Job 字段更新和 CronRun 磁盘历史：canceled /
	// overlap_skipped / job-deleted-mid-execute 三种 transient 终态都不应
	// 污染 Job 快照或 runs/<jobID>/。Metrics + WS broadcast 不受影响——
	// dashboard 必须能看到 skipped/canceled 帧。
	skipPersist bool
	prompt      string
	workDir     string
	fresh       bool
	// endedAt, when non-zero, overrides the s.now() read inside finishRun so
	// observeSuccessLatency and finishRun share one clock read (step-based
	// test clocks would otherwise advance an extra tick).
	endedAt time.Time
	// finalizer 是 caller 栈上的 *runFinalizer。finishRun 在 emitRunEnded 之前
	// 调 finalizer.finalize() 让 CurrentRun(jobID) 与 broadcast 同步 ok=false；
	// caller 的 defer 再调一次作兜底，done bool 保证只清理一次且只动本 run 的
	// gate。emitOverlapSkipped 必须传 nil（其 inflight gate 归并发 run 拥有）。
	finalizer *runFinalizer
	// replayOf links this run to the original it replayed (agentcore §7.3);
	// "" for normal runs.
	replayOf string
	// sandboxMeta is the cloud-execution receipt for placement=sandbox runs
	// (RFC §7.3); nil for local runs, whose record then carries no sandbox_meta.
	sandboxMeta *SandboxRunMeta
	// costInc is the LOCAL run's spend (session CostTotals after − before);
	// sandbox runs leave it zero and carry cost via sandboxMeta.
	costInc costledger.Increment
	// sandbox marks a placement=sandbox run so bumpRunStateMetrics also
	// advances the CronSandboxRun{Failed,TimedOut}Total buckets (#2173).
	// Deliberately separate from sandboxMeta != nil: pre-invoke failures
	// carry no receipt yet are still sandbox runs.
	sandbox bool
}

// finishRun is the single terminal hook for every cron execution path.
// It centralises:
//   - per-state metrics increment (CronRun*Total)
//   - persistent state write via recordTerminalResult (success / non-canceled error)
//   - cron_run_ended WS broadcast
//   - JobRunCounters bump (under s.mu, alongside recordTerminalResult)
//
// Adding a new error class is one mapping plus one finishArgs literal at the
// call site.
func (s *Scheduler) finishRun(a finishArgs) {
	// Defensive nil-job guard (#837): a panic here would be swallowed by
	// robfig's Recover ABOVE this frame, skipping finalize() + emitRunEnded and
	// leaving an orphaned "running" badge forever. Finalize this run's gate
	// and bail loudly instead.
	if a.job == nil {
		slog.Error("cron: finishRun called with nil job; finalizing inflight gate and skipping terminal protocol",
			"run_id", a.runID, "state", string(a.state), "err_class", string(a.errClass))
		a.finalizer.finalize()
		return
	}
	// endedAt via the injected clock (#643) so DurationMS is deterministic
	// under a fake clock; reuse the caller's pre-computed value when set so
	// step-based clocks are not advanced twice.
	endedAt := a.endedAt
	if endedAt.IsZero() {
		endedAt = s.now()
	}
	durationMS := endedAt.Sub(a.startedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0 // monotonic clock skew safety
	}

	// jobPersistOK=false → Job 字段回滚（marshal 失败）或 Job 已被并发删除，
	// 此时不得再写 CronRun history（list 读 Job 字段、timeline 读 CronRun，
	// 必须同步可见或同步缺失）。SECURITY: 落盘与 WS 广播只能用 persistedResult /
	// persistedErrMsg（已 redact + sanitise），绝不用原始 a.result / a.errMsg——
	// 错误串里的绝对路径会把工作区布局泄漏给所有 dashboard 客户端。
	persistedResult := a.result
	persistedErrMsg := a.errMsg
	jobPersistOK := false
	if !a.skipPersist {
		persistedResult, persistedErrMsg, jobPersistOK = s.recordTerminalResult(a.job, a.result, a.errMsg, a.sessionID, a.errClass, a.state, endedAt)
	} else {
		persistedResult = sanitiseRunResult(persistedResult)
		persistedErrMsg = sanitiseRunErrMsg(persistedErrMsg)
	}

	// Bump per-state metric only AFTER persistence settles, so a marshal-failure
	// rollback never leaves CronRunSucceededTotal +1 against a reverted Job.
	// skipPersist paths bump unconditionally (no Job rollback is possible; the
	// metric is their only durable record). The sandbox buckets sit behind the
	// SAME gate (#2173) so they remain a strict subset of the generic ones.
	if a.skipPersist || jobPersistOK {
		s.bumpRunStateMetrics(a.state, a.sandbox)
	}

	// CronRun history 写盘条件：skipPersist=false、jobPersistOK=true、runStore
	// 启用。两步写盘非事务（#992）：先 cron_jobs.json 成功才 Append runs/；崩溃
	// 只可能让 Job 计数领先一条而 runs/ 缺最新一条（over-report，下次 run 自愈），
	// 反方向结构上不可能。a.prompt 在此再过一次 SanitizeForLog（#1094）：旧
	// cron_jobs.json 可能带 C0/C1/bidi 的 legacy Prompt。
	persistedPrompt := osutil.SanitizeForLog(a.prompt, MaxPromptBytes)
	// #2058 / #2479: recordTerminalResult released s.mu before returning, and
	// Append's dir create + write run outside jobLock, so a concurrent
	// DeleteJobByID could resurrect an orphaned runs/<jobID>/. Double check:
	// (1) pre-write jobStillExists skips the write; (2) post-write re-check
	// drops exactly the record we wrote (dropOrphanRun). Both pinned by tests.
	if s.finishRunPreAppendHook != nil {
		s.finishRunPreAppendHook(a.job.ID)
	}
	if !a.skipPersist && jobPersistOK && s.runStoreEnabled() && s.jobStillExists(a.job.ID) {
		s.appendRun(&CronRun{
			RunID:      a.runID,
			JobID:      a.job.ID,
			State:      a.state,
			Trigger:    a.trigger,
			StartedAt:  a.startedAt,
			EndedAt:    endedAt,
			DurationMS: durationMS,
			SessionID:  a.sessionID,
			Prompt:     persistedPrompt,
			WorkDir:    a.workDir,
			Fresh:      a.fresh,
			Result:     persistedResult,
			// ResultBytes is the STORED byte count (post-truncate/redact/sanitise),
			// not the raw Claude output size — on-disk footprint only, matching
			// what the dashboard renders (#1910).
			ResultBytes: len(persistedResult),
			ErrorClass:  a.errClass,
			ErrorMsg:    persistedErrMsg,
			ReplayOf:    a.replayOf,
			// nil for local runs, so their record carries no sandbox_meta key.
			SandboxMeta: a.sandboxMeta,
			// local-run cost increment; 0 for sandbox runs, which report via SandboxMeta.
			CostUSD: a.costInc.USD,
		})
		// #2479 (2): post-write re-check; see above.
		if !s.jobStillExists(a.job.ID) {
			s.runStore.dropOrphanRun(a.job.ID, a.runID)
			slog.Info("cron run: job deleted during history write; dropped orphan run record",
				"job_id", a.job.ID, "run_id", a.runID)
		}
		// A new run record may introduce a SessionID the cache does not know
		// about; drop the snapshot so the next KnownSessionIDs() call rebuilds.
		s.invalidateKnownSessionsCache()
	}

	// The ledger is independent of run-record persistence: money was spent
	// even when the record is skipped (cancel / skipPersist paths included),
	// dropped as orphan, or has no store.
	s.appendLedger(a)

	// Finalize before the broadcast (#689) so a dashboard list arriving
	// concurrently with cron_run_ended observes CurrentRun(jobID) == ok:false.
	// The finalizer is per-run stack-local: the executeOpt defer fires second as
	// a no-op and can never reset a racing run-B's freshly-installed metadata.
	// Broadcast last so hub locks aren't held while we hold s.mu.
	a.finalizer.finalize()

	s.emitRunEnded(RunEndedEvent{
		JobID:      a.job.ID,
		RunID:      a.runID,
		State:      a.state,
		StartedAt:  a.startedAt,
		EndedAt:    endedAt,
		DurationMS: durationMS,
		SessionID:  a.sessionID,
		ErrorClass: a.errClass,
		ErrorMsg:   persistedErrMsg,
		Trigger:    a.trigger,
	})
	metrics.CronRunEndedTotal.Add(1)
}

// sanitiseRunResult applies the same rune truncation + secret redaction +
// SanitizeForLog pipeline that recordTerminalResult uses, factored out so the
// skipPersist path of finishRun reaches byte-identical output without touching
// s.mu. SanitizeForLog's byte cap is extended by len(truncatedSuffix) so a
// just-appended "…[truncated]" suffix is not byte-clipped.
func sanitiseRunResult(s string) string {
	s = truncateWithSuffix(s, maxStoredResultRunes)
	// Redact BEFORE SanitizeForLog so a leaked token never lands on disk or the
	// WS broadcast (#1006). Idempotent: [REDACTED] starts with no registered
	// prefix.
	s = redactSecretsInResult(s)
	return osutil.SanitizeForLog(s, maxStoredResultRunes+len(truncatedSuffix))
}

// localizeNotice is the canonical notice-pipeline helper: sanitise (truncate /
// redact secrets) then localize API-error envelopes before text reaches any IM
// channel. Single definition shared by scheduler_run.go and sandbox.go so the
// privacy-critical pipeline cannot diverge between call sites.
func localizeNotice(s string) string { return apierr.Localize(sanitiseRunResult(s)) }

// sanitiseRunErrMsg applies the cron error-redaction + log-injection scrub used
// by recordTerminalResult, for skipPersist branches whose error strings still
// flow into WS broadcasts and must not leak filesystem paths.
func sanitiseRunErrMsg(s string) string {
	s = redactPathsInCronError(s)
	// Secrets BEFORE SanitizeForLog, mirroring sanitiseRunResult.
	s = redactSecretsInResult(s)
	return osutil.SanitizeForLog(s, maxCronErrMsgRunes)
}

// emitOverlapSkipped runs the full RunStarted→finishRun lifecycle for a
// CAS-rejected execution attempt (a tick or TriggerNow that lost the
// concurrency gate to an in-flight run of the same job). It emits BOTH a
// RunStarted event AND drives finishRun (RunEnded) with RunStateSkipped +
// ErrClassOverlapSkipped: the started→ended pair is load-bearing for
// subscriber state machines (dashboard timeline / drawer), so a metric-only
// or ended-only variant is not acceptable (#521). finishRun's skipPersist
// keeps the synthetic run off disk. Kept as a named helper (#747) so future
// CAS-style guards reuse the same lifecycle contract.
func (s *Scheduler) emitOverlapSkipped(j *Job, viaTriggerNow bool) {
	s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassOverlapSkipped, "previous run still in flight", "overlap-skipped")
}

// emitSyntheticSkipped synthesises a started→ended pair for a CAS-bypassing
// guard that rejects a tick before any inflight metadata is populated. Used by
// emitOverlapSkipped and the router=nil short-circuit in executeOpt (#1323) so
// dashboards see the same lifecycle frames as a real run, with errClass
// distinguishing why the run never reached spawn. logTag distinguishes the
// slog message on the rare RunID-mint failure path.
func (s *Scheduler) emitSyntheticSkipped(j *Job, viaTriggerNow bool, errClass ErrorClass, errMsg, logTag string) {
	runID, err := generateRunID()
	if err != nil {
		// rand failure: suppressing the WS frame beats panicking from the cron
		// tick goroutine (#706); the underlying guard's slog.Error still shows.
		slog.Error("cron: failed to generate run ID for synthetic skipped event; suppressing",
			"job_id", j.ID, "trigger_now", viaTriggerNow, "err_class", string(errClass), "tag", logTag, "err", err)
		return
	}
	// Injected clock so a fake clock drives deterministic startedAt/endedAt.
	startedAt := s.now()
	trigger := TriggerScheduled
	if viaTriggerNow {
		trigger = TriggerManual
	}
	s.emitRunStarted(RunStartedEvent{
		JobID:     j.ID,
		RunID:     runID,
		StartedAt: startedAt,
		Trigger:   trigger,
	})
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: startedAt, trigger: trigger,
		state: RunStateSkipped, errClass: errClass,
		errMsg: errMsg, skipPersist: true,
	})
}

// JobState is the runtime-mutable terminal-result half of the Job struct: the
// LastRunAt / LastResult / LastError / LastErrorClass / LastSessionID /
// RunCounters cluster that every finishRun rewrites. It is a SEPARATE type from
// Job's wire-config fields so the runtime-state field set is enumerated in
// exactly one place; capture (Job.snapshotResultState) and rollback (restore)
// both route through it without changing the on-disk JSON shape (#764).
//
// restore re-applies the captured values to j; caller MUST hold s.mu.
type JobState struct {
	LastRunAt      time.Time
	LastResult     string
	LastError      string
	LastErrorClass ErrorClass
	LastSessionID  string
	Counters       JobRunCounters
}

func (p JobState) restore(j *Job) {
	j.LastRunAt = p.LastRunAt
	j.LastResult = p.LastResult
	j.LastError = p.LastError
	j.LastErrorClass = p.LastErrorClass
	j.LastSessionID = p.LastSessionID
	j.RunCounters = p.Counters
}

// snapshotResultState captures the runtime-mutable terminal-result state into
// a JobState. Caller must hold s.mu. Paired with restore so adding a
// runtime-state field is a two-site edit rather than a hunt across mutation
// paths.
func (j *Job) snapshotResultState() JobState {
	return JobState{
		LastRunAt:      j.LastRunAt,
		LastResult:     j.LastResult,
		LastError:      j.LastError,
		LastErrorClass: j.LastErrorClass,
		LastSessionID:  j.LastSessionID,
		Counters:       j.RunCounters,
	}
}

// recordTerminalResult persists the terminal result (LastResult / LastError /
// LastErrorClass / Counters) for non-skipPersist paths and returns the
// post-sanitised (result, errMsg) pair so finishRun can reuse byte-identical
// content in the CronRun history record.
//
// Returns ok=false when the Job was deleted between snapshot and this call, or
// when marshal/persist failed and the Job fields were rolled back in-memory.
// In both cases the caller MUST also skip the CronRun history record so the
// dashboard list (Job fields) and timeline (CronRun) never diverge.
func (s *Scheduler) recordTerminalResult(j *Job, result, errMsg, sessionID string, errClass ErrorClass, state RunState, endedAt time.Time) (string, string, bool) {
	// truncateWithSuffix is the single source of truth for the rune trim +
	// …[truncated] suffix; this path and sanitiseRunResult must stay
	// byte-identical.
	result = truncateWithSuffix(result, maxStoredResultRunes)
	// Order invariant on both branches: redact secrets (and paths for errMsg)
	// THEN SanitizeForLog, so a token's surrounding control bytes are still
	// stripped and no plaintext token reaches Job.LastResult / LastError →
	// cron_jobs.json or the WS broadcast (#1006).
	result = redactSecretsInResult(result)
	errMsg = redactSecretsInResult(errMsg)
	errMsg = redactPathsInCronError(errMsg)
	// Extend SanitizeForLog's byte cap by the suffix length so an
	// already-truncated result keeps its trailing marker intact.
	result = osutil.SanitizeForLog(result, maxStoredResultRunes+len(truncatedSuffix))
	errMsg = osutil.SanitizeForLog(errMsg, maxCronErrMsgRunes)

	// The critical section runs under a single deferred Unlock inside an IIFE
	// so any exit path (incl. panic) releases s.mu. Only the Job field mutation
	// and a detached value-copy snapshot happen under the lock; json.Marshal
	// runs in persistSnapshot OFF the lock so a large encode does not serialise
	// the dashboard read path on every tick (#1923).
	var (
		save           func()
		sessionChanged bool
		snap           jobsSnapshot
		haveSnap       bool
		prev           JobState
	)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.jobs[j.ID]; !exists {
			return
		}
		prev = j.snapshotResultState()

		j.LastRunAt = endedAt
		j.LastResult = result
		j.LastError = errMsg
		j.LastErrorClass = errClass
		if sessionID != "" {
			j.LastSessionID = sessionID
		}
		j.RunCounters.addRun(state)

		snap = s.snapshotJobsForSaveLocked()
		haveSnap = true
		// Detect whether LastSessionID changed under the lock so the
		// KnownSessionIDs TTL cache is invalidated exactly when the set shifted.
		sessionChanged = sessionID != "" && sessionID != prev.LastSessionID
	}()

	if !haveSnap {
		return result, errMsg, false
	}

	// Marshal OFF the lock (#1923). On failure re-acquire s.mu and roll back the
	// in-memory mutation so live reads and the on-disk snapshot stay in sync.
	// The brief window where the unpersisted mutation is visible is acceptable:
	// finishRun gates cron_run_ended on this function's ok return.
	saveFn, perr := s.persistSnapshot(snap)
	if perr != nil {
		s.mu.Lock()
		if _, exists := s.jobs[j.ID]; exists {
			prev.restore(j)
		}
		s.mu.Unlock()
		slog.Warn("cron: recordTerminalResult persist failed; in-memory result reverted",
			"job_id", j.ID, "err", perr)
		return result, errMsg, false
	}
	save = saveFn

	if sessionChanged {
		s.invalidateKnownSessionsCache()
	}
	save()
	return result, errMsg, true
}

// redactAddrRe matches IPv4 address + optional port in error messages such as
// "dial tcp 192.168.1.5:4012: connection refused". Hostnames are not matched:
// they would need a DNS lookup; IP literals are structurally identifiable.
var redactAddrRe = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b`)

// redactAddrIPv6Re matches bracketed IPv6 addresses + optional port ("dial tcp
// [2001:db8::1]:4012"). Only the bracket form is matched — bare IPv6 is
// ambiguous in free-form text. At least one colon is required inside the
// brackets so [foo] / [1] are not over-redacted; the leading hex group is `*`
// so [::1] / [::] still match. That admits the degenerate `[:]`, which is why
// redaction goes through ReplaceAllStringFunc + ipv6BracketIsAddr below.
var redactAddrIPv6Re = regexp.MustCompile(`\[[0-9a-fA-F]*:[0-9a-fA-F:]+\](:\d+)?`)

// ipv6BracketIsAddr reports whether a redactAddrIPv6Re match is a plausible
// IPv6 literal rather than a degenerate token like "[:]" or "[a:b]": it needs
// either a "::" compression run or at least two colons. Single-colon forms
// look like host:port and must not be over-redacted. The input is a full match
// including brackets and optional :port suffix.
func ipv6BracketIsAddr(match string) bool {
	end := strings.IndexByte(match, ']')
	if end < 0 {
		return false
	}
	body := match[1:end] // strip leading '[' and trailing ']'
	if strings.Contains(body, "::") {
		return true
	}
	// Require at least 2 colons (e.g. "a:b:c"); single-colon "[a:b]" is not IPv6.
	n := 0
	for i := 0; i < len(body); i++ {
		if body[i] == ':' {
			n++
			if n >= 2 {
				return true
			}
		}
	}
	return false
}

// hasAddrTrigger is a zero-alloc fast-path check: true only when s contains a
// digit immediately followed by a dot (dotted-quad IPv4) or a '[' (bracket-form
// IPv6). When false the regexes are skipped entirely — common cron error
// classes ("context deadline exceeded", "permission denied") take this path.
func hasAddrTrigger(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '[' {
			return true
		}
		if i >= 1 && s[i] == '.' && s[i-1] >= '0' && s[i-1] <= '9' {
			return true
		}
	}
	return false
}

// redactAddrInCronError replaces IPv4(:port)? and [IPv6](:port)? patterns
// with [redacted-addr].
// Fast-path: returns s unmodified (zero alloc) when hasAddrTrigger is false.
func redactAddrInCronError(s string) string {
	if !hasAddrTrigger(s) {
		return s
	}
	s = redactAddrRe.ReplaceAllString(s, "[redacted-addr]")
	s = redactAddrIPv6Re.ReplaceAllStringFunc(s, func(m string) string {
		if ipv6BracketIsAddr(m) {
			return "[redacted-addr]"
		}
		return m
	})
	return s
}

// redactPathsBuilderPool reuses strings.Builder scratch space across
// redactPathsInCronError slow-path invocations (hot: every cron tick + every
// TriggerNow). Empty / no-path fast-path inputs do not touch the pool. The pool
// only elides the Builder + initial backing-slice alloc; b.String() still
// copies, which is unavoidable for any non-aliasing implementation (#872).
var redactPathsBuilderPool = sync.Pool{
	New: func() any {
		// 512B initial capacity: most cron error messages are small.
		b := &strings.Builder{}
		b.Grow(512)
		return b
	},
}

// redactPathsBuilderPoolMaxCap drops oversized buffers from the pool so a
// near-maxRedactErrLen input does not pin memory for the process lifetime.
// Sized at 4× maxRedactErrLen to allow worst-case Grow(len(s)) headroom
// without recycling.
const redactPathsBuilderPoolMaxCap = 4 * maxRedactErrLen

// hasNoPathTrigger reports whether s contains none of the three bytes that can
// begin a redactable path token: a POSIX slash, a Windows backslash, or a
// tilde-home shorthand. Shared by both fast-path gates in
// redactPathsInCronError so the trigger-byte set cannot desync (#850); the scan
// itself lives in osutil so sysession reuses the same policy (#983).
func hasNoPathTrigger(s string) bool {
	return osutil.HasNoPathTrigger(s)
}

// redactPathsInCronError strips absolute filesystem paths (POSIX `/abs`,
// Windows `C:\…` / `C:/…`, home-relative `~/`) and IP:port literals from a cron
// execution error message before persistence, so "session error: workspace
// …/repo/x: permission denied" does not enumerate the operator's filesystem to
// every dashboard viewer. Token-wise scan (no regex compile per run). UNC paths
// (`\\server\share`) are intentionally out of scope — see #952.
func redactPathsInCronError(s string) string {
	if s == "" {
		return s
	}
	// Hot fast-path: short no-path-trigger strings skip truncation AND the
	// Builder pool and are returned aliased (#1115). IP:port redaction still
	// runs — "dial tcp 192.168.1.5:4012" has no slash so hasNoPathTrigger is
	// true, and the addr would otherwise leak through.
	if len(s) <= redactFastPathMaxLen && hasNoPathTrigger(s) {
		return redactAddrInCronError(s)
	}
	// Byte-level cap split on a rune boundary — naked s[:maxRedactErrLen] can
	// fall mid-codepoint (CJK error messages), producing invalid UTF-8 that
	// poisons cron_jobs.json.
	if len(s) > maxRedactErrLen {
		n := textutil.TruncateAtRuneBoundary(s, maxRedactErrLen)
		s = s[:n] + "…"
	}
	// No path-shaped bytes after truncation → skip the Builder; addr redaction
	// still applies.
	if hasNoPathTrigger(s) {
		return redactAddrInCronError(s)
	}
	b := redactPathsBuilderPool.Get().(*strings.Builder)
	// Reset BEFORE Grow on the pooled instance: strings.Builder.Reset() drops
	// the internal slice entirely, and without it the prior call's residual
	// bytes would prefix this output. Only the *Builder header comes from the
	// pool; the backing []byte and the final String() still allocate per call.
	defer func() {
		// Drop oversized buffers so a one-off near-maxRedactErrLen input
		// does not pin memory for the process lifetime.
		if b.Cap() > redactPathsBuilderPoolMaxCap {
			return
		}
		b.Reset()
		redactPathsBuilderPool.Put(b)
	}()
	b.Reset()
	b.Grow(len(s))
	// Path detection policy (POSIX / Windows drive / ~/ home, bare-root
	// pass-through, whitespace/`:` delimiters) lives in osutil so sysession can
	// reuse it (#983); writing into the pooled Builder keeps cron's alloc profile.
	osutil.RedactAbsolutePathsInto(b, s)
	// Strip IP:port patterns that survive the path pass (no slash/backslash/tilde).
	return redactAddrInCronError(b.String())
}
