// scheduler_finish.go: terminal hooks for every cron execution path
// (write side) plus run-history queries the dashboard reads (read side).
//
// Centralising the finish path here keeps the seven branches of executeOpt
// converging on a single struct literal (finishArgs) and lets the dashboard
// query API (CurrentRun / ListRuns / RecentRuns / GetRun) live next to the
// writers that produce the records — when the schema of CronRun changes,
// readers and writers move together. No behaviour change. Methods stay on
// *Scheduler so the s.mu / s.jobs / s.runStore / s.runningJobs fields
// remain accessible without exporting.

package cron

import (
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// CurrentRun returns the inflight snapshot for jobID, or (zero, false) when
// the job is not currently executing. Used by the dashboard list API to
// show "running 12s" badges.
func (s *Scheduler) CurrentRun(jobID string) (runInflightView, bool) {
	v, ok := s.runningJobs.Load(jobID)
	if !ok {
		return runInflightView{}, false
	}
	// Defensive: runningJobs is sync.Map[string]*runInflight by contract,
	// but the type-erased Load makes a future refactor that stores a
	// different type or a nil value silently panic here. The two-value
	// assertion + nil check turns that into a graceful "no inflight".
	inf, ok := v.(*runInflight)
	if !ok || inf == nil {
		return runInflightView{}, false
	}
	return inf.snapshot()
}

// RunInflightView is the exported shape for CurrentRun's snapshot,
// surfaced by server-side handlers building the list / detail JSON
// response. Kept here (cron package) so the field set stays single-
// sourced; the server view re-marshals into its own wire shape.
type RunInflightView = runInflightView

// ListRuns returns up to limit CronRunSummary entries for jobID, newest
// first. before is a cutoff (only runs with StartedAt < before); zero
// means "no cutoff" (latest page).
//
// Safe to call when persistence is disabled (StorePath empty): returns
// nil. The dashboard list endpoint and detail endpoint both go through
// this method so the runs/ schema stays opaque to server/.
func (s *Scheduler) ListRuns(jobID string, limit int, before time.Time) []CronRunSummary {
	if s == nil || s.runStore == nil {
		return nil
	}
	return s.runStore.List(jobID, limit, before)
}

// RecentRuns is the convenience wrapper for the cron list view's
// recent_runs field. Cap is enforced inside ListRuns.
func (s *Scheduler) RecentRuns(jobID string, n int) []CronRunSummary {
	if s == nil || s.runStore == nil {
		return nil
	}
	return s.runStore.Recent(jobID, n)
}

// GetRun returns the full CronRun for runID under jobID. Returns
// (nil, fs.ErrNotExist) when missing; (nil, ErrCorruptRun) when present
// but unusable. Server layer maps these to 404 / 500 respectively.
func (s *Scheduler) GetRun(jobID, runID string) (*CronRun, error) {
	if s == nil || s.runStore == nil {
		return nil, fs.ErrNotExist
	}
	return s.runStore.Get(jobID, runID)
}

// finishArgs bundles the parameters of finishRun so each call site reads
// as a struct literal — many fields are optional (errClass / errMsg / sessionID
// / result / skipPersist) and a positional signature would be brittle.
//
// snapshot fields (prompt/workDir/fresh) are populated only on paths that
// have already taken the snapshotJob() — overlapSkipped / pre-snapshot
// preflight failures pass them as zero values, which CronRun renders as
// empty (the dashboard will fall back to Job.Prompt for display).
type finishArgs struct {
	// job 是终结的目标 Job。state==Skipped 的 overlap 路径仍要传 *Job
	// 因为 emitRunEnded 需要 Job.ID 作为事件 key（其余字段由 finishRun 构造
	// CronRun 时填）；DeleteJob 中途的竞态由 recordResultP0WithSanitised 内
	// jobs[id] 二次校验。
	job *Job
	// runID / startedAt 与上游 emitRunStarted 的 RunStartedEvent 一一对
	// 应；finishRun 据此发 RunEnded，订阅方 (dashboard hub) 用 RunID 配
	// 对 started→ended 帧。
	runID     string
	startedAt time.Time
	// trigger 与 RunStartedEvent.Trigger 必须一致；errMsg/result 经过
	// sanitiseRunResult / redactPathsInCronError 流水线后才会进 ws/disk。
	trigger TriggerKind
	// state 决定 metrics 计数桶 + 是否进 succeeded/failed counters。
	// Skipped 不计入 Failed（dashboard "失败率" 排除 overlap 噪音）。
	state RunState
	// sessionID 是 GetOrCreate 分配的 CLI session_id（fresh=true 路径
	// 必为空字符串——CAS 进入 spawn 但还未 GetOrCreate；持久化模式下
	// 是上一次的 session_id）。空值 dashboard 隐藏「打开会话」按钮。
	sessionID string
	// result 是 CLI 末轮文本输出（已经 RFC §6 的 sanitiseRunResult，包
	// 括 4K rune 截断 + …[truncated] 后缀 + SanitizeForLog 控制字符过滤）。
	result string
	// errClass 是机器可读的错误分类（PreflightFailed / WorkDirUnreachable
	// / Canceled / Timeout / SpawnFailed / SendFailed / OverlapSkipped 等）。
	// dashboard 用它选图标 + i18n 文案；errMsg 仅作展开详情。
	errClass ErrorClass
	// errMsg 是人类可读错误（ASCII 控制符已 escape，绝对路径已 redact）。
	// 严格 ≤ maxCronErrMsgRunes (512 runes)，超长被 SanitizeForLog 截断。
	errMsg string
	// skipPersist 同时控制两件事：跳过 Job 字段更新（LastRunAt/LastResult/
	// LastError/LastErrorClass/Counters）和跳过 CronRun 磁盘历史。当前所有
	// 调用点这两件事都同步：canceled / overlap_skipped / job-deleted-mid-
	// execute 三种 transient 终态 — 都不应该污染 Job 快照，也不应该塞进
	// runs/<jobID>/。如果将来要独立控制（比如"想记历史但不更新 Counters"），
	// 拆成 skipJobUpdate / skipHistoryRecord 两个 bool；当前合一是 RFC §5
	// 状态机表的直接映射。Metrics + WS broadcast 不受 skipPersist 影响——
	// 故意如此，dashboard 必须能看到 skipped/canceled 帧。R220-ARCH-1.
	skipPersist bool
	prompt      string
	workDir     string
	fresh       bool
	// finalizer 是 caller 栈上的 *runFinalizer。finishRun 在 emitRunEnded
	// 之前调 finalizer.finalize() 让 CurrentRun(jobID) 与 broadcast 同步
	// ok=false；caller 自己的 defer 也调一次作兜底（覆盖 jitter-window
	// 早返路径）。done bool 保证两次调用只清理一次，并且因为 finalizer
	// 是 per-run 栈对象，run-A 的 defer 只会看到 run-A 的 done=true，
	// 永远不会动到 run-B 已抢占的 *runInflight 字段。emitOverlapSkipped
	// 必须传 nil（它的 inflight gate 归并发 run 拥有，不应在 overlap
	// 路径释放）。R246-GO-3 (#689).
	finalizer *runFinalizer
}

// finishRun is the single terminal hook for every cron execution path.
// It centralises:
//   - per-state metrics increment (CronRun*Total)
//   - persistent state write via recordResult (success / non-canceled error)
//   - cron_run_ended WS broadcast
//   - JobRunCounters bump (under s.mu, alongside recordResult)
//
// Centralising avoids the historical pattern of recordResult-and-deliver-and-
// log scattered across executeOpt's seven branches; adding a new error class
// is now one mapping plus one finishArgs literal at the call site.
func (s *Scheduler) finishRun(a finishArgs) {
	endedAt := time.Now()
	durationMS := endedAt.Sub(a.startedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0 // monotonic clock skew safety
	}

	// Persist (LastRunAt/LastResult/LastError/Counters) for terminal paths
	// that historically updated state. Canceled / shutdown paths skipPersist
	// to preserve "next start retries" semantics; same paths also skip the
	// CronRun history record (transient by definition; would inflate runs/
	// with shutdown noise).
	//
	// SECURITY: persistedResult / persistedErrMsg are post-redact + post-
	// sanitise strings. Both the on-disk CronRun and the WS broadcast must
	// use these — never the raw a.result / a.errMsg — otherwise an error
	// containing an absolute filesystem path (e.g. "session error: open
	// /home/ops/private-repo: permission denied") leaks the workspace
	// layout to every authenticated dashboard client. R220-SEC-1.
	//
	// On the skipPersist path recordResultP0WithSanitised is bypassed, so
	// we apply the same redact + sanitise pipeline inline. Cheap (regex-
	// free path scan + ASCII control filter) and ensures no broadcast
	// branch can echo raw err.Error() / fmt.Sprintf output to clients.
	//
	// jobPersistOK 表示 Job 字段 + cron_jobs.json 落盘是否真的成功。
	// false → marshal 失败回滚了 Job in-memory 字段，或者 Job 已被并发
	// 删除。两种情况下都不该再写 CronRun history（dashboard list 读
	// Job 字段，timeline 读 CronRun，二者必须同步可见或同步缺失）。
	// 这是 R220-ARCH-2 一致性窗口的修复。
	persistedResult := a.result
	persistedErrMsg := a.errMsg
	jobPersistOK := false
	if !a.skipPersist {
		persistedResult, persistedErrMsg, jobPersistOK = s.recordTerminalResult(a.job, a.result, a.errMsg, a.sessionID, a.errClass, a.state)
	} else {
		persistedResult = sanitiseRunResult(persistedResult)
		persistedErrMsg = sanitiseRunErrMsg(persistedErrMsg)
	}

	// R230C-GO-8: bump per-state metric AFTER persistence settles. Previous
	// ordering bumped pre-persist, so a marshal-failure rollback still left
	// CronRunSucceededTotal +1 even though Job state had been reverted, with
	// dashboards over-reporting throughput vs durable runs.
	//
	// Skip-persist paths (canceled / shutdown / overlap-skipped) still bump
	// because by definition no Job rollback is possible — the metric is the
	// only durable record those runs leave. Persist-attempted paths bump
	// only when jobPersistOK == true.
	if a.skipPersist || jobPersistOK {
		s.bumpRunStateMetrics(a.state)
	}

	// CronRun history (P1). Conditions:
	//   - skipPersist=false（这次 run 应该被记录）
	//   - jobPersistOK=true（Job 端写盘成功；否则 disk-divergence 风险）
	//   - runStore 启用
	//
	// R250-SEC-5 (#1094): a.prompt is the snapshot Job.Prompt at execute
	// time. New jobs flow through containsCronUnsafe / validateCronPrompt
	// at the dashboard / IM write edge AND a defence-in-depth scan inside
	// loadJobs. But a cron_jobs.json predating those gates can carry a
	// legacy Prompt with C0 / C1 / bidi runes — every CronRun.Prompt
	// persisted thereafter inherits them, landing in operator-side log
	// scrapers and SIEMs that read runs/<jobID>/<runID>.json directly.
	// Run the same SanitizeForLog scrub at the persist boundary so the
	// stored record matches what handleRunDetail (read-side) would produce.
	// Idempotent on already-clean prompts; cheap relative to JSON marshal +
	// fsync that immediately follow.
	persistedPrompt := osutil.SanitizeForLog(a.prompt, MaxPromptBytes)
	if !a.skipPersist && jobPersistOK && s.runStore != nil {
		s.runStore.Append(&CronRun{
			RunID:       a.runID,
			JobID:       a.job.ID,
			State:       a.state,
			Trigger:     a.trigger,
			StartedAt:   a.startedAt,
			EndedAt:     endedAt,
			DurationMS:  durationMS,
			SessionID:   a.sessionID,
			Prompt:      persistedPrompt,
			WorkDir:     a.workDir,
			Fresh:       a.fresh,
			Result:      persistedResult,
			ResultBytes: len(persistedResult),
			ErrorClass:  a.errClass,
			ErrorMsg:    persistedErrMsg,
		})
		// R250-PERF-7: a new run record may introduce a SessionID the
		// cache does not know about; drop the snapshot so the next
		// KnownSessionIDs() call rebuilds.
		s.invalidateKnownSessionsCache()
	}

	// Broadcast last so server-side hub locks aren't held while we hold s.mu.
	// ErrorMsg uses persistedErrMsg (post-redact, post-sanitise) — see the
	// SECURITY note above for why a.errMsg is never used here.
	//
	// R246-GO-3 (#689): finalize before the broadcast so a dashboard list
	// arriving concurrently with cron_run_ended observes CurrentRun(jobID)
	// == ok:false rather than the stale runInflightView{Phase:Spawning}
	// the defer would otherwise leave until executeOpt returns. The
	// finalizer is per-run stack-local; finishRun fires it first, the
	// executeOpt defer fires it second as a no-op (done flag set). Run-A's
	// defer can NEVER reset run-B's freshly-installed metadata because
	// run-A's done=true short-circuits run-A's defer regardless of whether
	// a racing run-B has won the next CAS — the gate isolation comes from
	// per-run finalizer identity, not from any atomic on *runInflight.
	// emitOverlapSkipped passes nil here (its inflight gate belongs to
	// the concurrent owning run we must not release).
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

// sanitiseRunResult applies the same rune truncation + SanitizeForLog
// pipeline that recordResultP0WithSanitised uses, factored out so the
// skipPersist path of finishRun can reach the same byte-output without
// touching s.mu / persistJobsLocked. Idempotent w.r.t. clean strings.
//
// truncateWithSuffix (limits.go) handles the rune trim + suffix; we extend
// SanitizeForLog's byte cap by len(truncatedSuffix) so a 4K-rune input that
// just got "…[truncated]" appended doesn't have its suffix byte-clipped on
// the way out. R232-PERF-9 / R234-CR-1.
func sanitiseRunResult(s string) string {
	s = truncateWithSuffix(s, maxStoredResultRunes)
	// R234-SEC-7 (#1006): scrub well-known secret-prefix patterns
	// (sk-ant-, ghp_, AKIA, …) BEFORE SanitizeForLog so a leaked token in
	// Claude output never lands on disk or the dashboard WS broadcast.
	// Idempotent: the [REDACTED] marker does not start with any registered
	// prefix, so re-running the redactor on a previously-scrubbed string
	// is a no-op. Mirrors redactPathsInCronError's call ordering on the
	// errMsg path (see recordTerminalResult below).
	s = redactSecretsInResult(s)
	return osutil.SanitizeForLog(s, maxStoredResultRunes+len(truncatedSuffix))
}

// sanitiseRunErrMsg applies the cron error-redaction + log-injection
// scrub used by recordResultP0WithSanitised, for skipPersist branches
// (canceled / shutdown / overlap-skipped) whose error strings still
// flow into WS broadcasts and must not leak filesystem paths.
func sanitiseRunErrMsg(s string) string {
	s = redactPathsInCronError(s)
	return osutil.SanitizeForLog(s, maxCronErrMsgRunes)
}

// emitOverlapSkipped runs the full RunStarted→finishRun lifecycle for a
// CAS-rejected execution attempt (a tick or TriggerNow that lost the
// concurrency gate to an already-in-flight run of the same job). Despite
// the "Skipped" terminology, this function emits BOTH a RunStarted event
// AND drives finishRun (which emits RunEnded), so subscribers see the
// same started→ended pair they would for a normal run; the state field
// carries RunStateSkipped + ErrClassOverlapSkipped so dashboards render
// it as a no-op pill instead of a real run timeline.
//
// CronRunStartedTotal (via emitRunStarted) and the per-state finished
// metric (via finishRun) both bump. The dual emit is intentional: it
// keeps the runs/<id> dashboard drawer renderable and prevents
// subscriber state machines from missing the "started" anchor when a
// manual TriggerNow collides with an in-flight run.
//
// The CAS gate trips before any inflight metadata is populated, so we
// synthesise a RunID + StartedAt locally; finishRun's skipPersist=true
// short-circuit keeps the synthetic run off disk (it only exists in the
// WS broadcast stream).
//
// R246-CR-013 (#747): kept as a named function despite having a single
// call site (executeOpt's CAS-fail branch in scheduler_run.go) for two
// reasons:
//
//  1. The 5-line synthesise-RunID + start + finish dance is a
//     non-trivial composition over emitRunStarted + finishRun + a
//     hand-built finishArgs literal. Inlining at the call site would
//     bury the "skipped runs go through the same lifecycle as real
//     runs" contract inside the executeOpt function, where it competes
//     for attention with the run-success/error happy path.
//  2. Future call sites are anticipated (not hypothetical): when other
//     CAS-style guards land — e.g. a per-workspace cap that rejects
//     spawn before reaching the per-job CAS, or a backpressure-driven
//     manual skip — they will need the same "emit started+ended pair
//     so subscriber timelines stay consistent" semantics. Having the
//     helper avoid copy-pasting the 5-line dance across each future
//     guard preserves the single-place edit point for the lifecycle
//     contract (e.g. if RunEvent gains a new required field).
//
// Reviewers tempted to inline this back into executeOpt: please add the
// new caller(s) first, then re-evaluate.
//
// R241-ARCH-13 (#521) proposed a lite-path (metric-only + single WS
// event) on the CAS-fail fast path to relieve hub lock pressure under
// bursty triggers. Won't-fix: the started→ended pair is load-bearing
// for subscriber state machines (dashboard run timeline, history-panel
// indexers, drawer rendering). Dropping the started frame would leave
// the dashboard with an "ended without started" frame that subscribers
// either drop (silent UX regression) or render as an orphan (misleading
// "skipped from nowhere" pill). The metric-only variant cannot replace
// the WS event without breaking the subscriber contract, and combining
// the two events into a single composite event would force a schema
// change in the cron WS protocol — disproportionate to the savings on
// a path that fires only when CAS already lost (i.e. the in-flight run
// is paying the dominant cost). Hub lock pressure is bounded by the
// per-job CAS itself: at most one overlap-skipped per tick per job.
func (s *Scheduler) emitOverlapSkipped(j *Job, viaTriggerNow bool) {
	s.emitSyntheticSkipped(j, viaTriggerNow, ErrClassOverlapSkipped, "previous run still in flight", "overlap-skipped")
}

// emitSyntheticSkipped synthesises a started→ended pair for a CAS-bypassing
// guard that rejects a tick before any inflight metadata is populated. Used
// by both emitOverlapSkipped (per-job CAS lost) and the router=nil
// short-circuit in executeOpt (R20260527122801-CR-13 #1323) so dashboards
// see the same lifecycle frames they would for a real run, with the
// errClass distinguishing why the run never reached spawn.
//
// logTag distinguishes the slog message on the rare RunID-mint failure
// path so operators can tell which guard tripped.
func (s *Scheduler) emitSyntheticSkipped(j *Job, viaTriggerNow bool, errClass ErrorClass, errMsg, logTag string) {
	runID, err := generateRunID()
	if err != nil {
		// R242-CR-14 (#706): rand failure on the synthetic-skip path is
		// already degraded; suppressing the WS frame is strictly better
		// than panicking from the cron tick goroutine. Operators still
		// see the underlying guard's slog.Error.
		slog.Error("cron: failed to generate run ID for synthetic skipped event; suppressing",
			"job_id", j.ID, "trigger_now", viaTriggerNow, "err_class", string(errClass), "tag", logTag, "err", err)
		return
	}
	startedAt := time.Now()
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

// jobResultSnapshot captures the terminal-result-relevant Job fields
// before recordTerminalResult mutates them, so a persistJobsLocked failure
// can roll the in-memory Job state back to the pre-mutation values without
// rebuilding the field list at the rollback site. R247-CR-14 (#586): the
// previous inline anonymous-struct literal duplicated field types between
// the snapshot capture and the rollback assignment, leaving every future
// "add a field to LastFoo" change two coupled edits to keep in sync.
//
// restore re-applies the captured values to j; caller MUST hold s.mu so
// the in-memory state stays serialised against concurrent readers.
type jobResultSnapshot struct {
	LastRunAt      time.Time
	LastResult     string
	LastError      string
	LastErrorClass ErrorClass
	LastSessionID  string
	Counters       JobRunCounters
}

func (p jobResultSnapshot) restore(j *Job) {
	j.LastRunAt = p.LastRunAt
	j.LastResult = p.LastResult
	j.LastError = p.LastError
	j.LastErrorClass = p.LastErrorClass
	j.LastSessionID = p.LastSessionID
	j.RunCounters = p.Counters
}

// recordTerminalResult persists the terminal result (LastResult /
// LastError / LastErrorClass / Counters) for non-skipPersist paths and
// returns the post-sanitised (result, errMsg) pair so finishRun can reuse
// the same byte content in the CronRun history record. The two outputs
// must remain byte-identical or the dashboard list would diverge from
// runs/<jobID>/<run_id>.json on disk.
//
// Returns ok=false in two failure modes:
//   - target Job has been deleted between snapshot and recordResult (race
//     with DeleteJobByID): caller should also skip the CronRun history
//     record because writing it would create a runs/<jobID>/ subtree for
//     a job that no longer exists in s.jobs.
//   - persistJobsLocked / marshal failed and we rolled back Job fields
//     in-memory: caller MUST also skip the CronRun history record so
//     dashboard list view (reads Job fields) and timeline view (reads
//     CronRun) don't diverge — they'd otherwise show contradictory state
//     for the same run. R220-ARCH-2.
//
// R220-GO-1 / R230B-SEC-1 / R232-ARCH-2: previously a thin recordResultP0
// wrapper existed for tests pinning the (j, result, errMsg, sessionID,
// errClass, state) signature. No production caller used it; finishRun goes
// direct. The wrapper was dead code and has been removed; tests assert on
// outcomes (Job fields, CronRun summary), not wrapper presence. The
// "double-track recordResult vs recordResultP0WithSanitised" smell flagged
// by R230B-SEC-1 (missing RunCounters.addRun + LastErrorClass on the dead
// path) and R232-ARCH-2 (sanitize-arg drift across the two paths) is
// therefore moot — only this single P0 path remains, and persist_failure_test
// (the last "test stub" caller) already invokes this function directly.
// Do NOT reintroduce a thinner wrapper without first checking those TODOs.
//
// R247-CR-14 / R247-CR-15 (#586): renamed from recordResultP0WithSanitised
// to drop the "P0" review-tag prefix that lost meaning once the dead
// recordResult path was deleted (R220-GO-1). The rollback state now lives
// in the named jobResultSnapshot struct above so a future "add a Last*
// field" diff lands once on the type instead of three coupled edits.
func (s *Scheduler) recordTerminalResult(j *Job, result, errMsg, sessionID string, errClass ErrorClass, state RunState) (string, string, bool) {
	// truncateWithSuffix (limits.go) is the single source of truth for the
	// rune-trim + …[truncated] suffix; both this path and sanitiseRunResult
	// must produce byte-identical output so the skipPersist branch of
	// finishRun and the disk record never disagree on visible content.
	// R234-CR-1 consolidated three open-coded copies into the helper.
	result = truncateWithSuffix(result, maxStoredResultRunes)
	// R234-SEC-7 (#1006): scrub well-known secret-prefix patterns
	// (sk-ant-, ghp_, AKIA, …) before the final SanitizeForLog so plaintext
	// tokens in Claude output do not flow into Job.LastResult →
	// cron_jobs.json or the dashboard WS broadcast. Mirrors the
	// redactPathsInCronError path-redaction step that already runs on the
	// errMsg branch — same ordering invariant: redact, THEN log-injection
	// scrub, so a token's surrounding text still has its control bytes
	// stripped.
	result = redactSecretsInResult(result)
	errMsg = redactPathsInCronError(errMsg)
	// Extend SanitizeForLog's byte cap by the suffix length so an
	// already-truncated result keeps the trailing marker intact;
	// otherwise byte-level truncation could clip mid-suffix.
	// R232-PERF-9.
	result = osutil.SanitizeForLog(result, maxStoredResultRunes+len(truncatedSuffix))
	errMsg = osutil.SanitizeForLog(errMsg, maxCronErrMsgRunes)

	s.mu.Lock()
	if _, ok := s.jobs[j.ID]; !ok {
		s.mu.Unlock()
		return result, errMsg, false
	}
	prev := jobResultSnapshot{
		LastRunAt:      j.LastRunAt,
		LastResult:     j.LastResult,
		LastError:      j.LastError,
		LastErrorClass: j.LastErrorClass,
		LastSessionID:  j.LastSessionID,
		Counters:       j.RunCounters,
	}

	j.LastRunAt = time.Now()
	j.LastResult = result
	j.LastError = errMsg
	j.LastErrorClass = errClass
	if sessionID != "" {
		j.LastSessionID = sessionID
	}
	j.RunCounters.addRun(state)

	save, perr := s.persistJobsLocked()
	if perr != nil {
		prev.restore(j)
		s.mu.Unlock()
		slog.Warn("cron: recordTerminalResult persist failed; in-memory result reverted",
			"job_id", j.ID, "err", perr)
		return result, errMsg, false
	}
	// R250-PERF-7: detect whether LastSessionID changed under the lock
	// so we can invalidate the KnownSessionIDs TTL cache exactly when
	// the persisted set has shifted. Comparing against the snapshot
	// taken before the in-place write avoids redundant invalidation
	// when the same session id repeats.
	sessionChanged := sessionID != "" && sessionID != prev.LastSessionID
	s.mu.Unlock()

	if sessionChanged {
		s.invalidateKnownSessionsCache()
	}
	save()
	// Phase D (RFC §3.5): the legacy s.onExecute hook fired here to
	// drive the cron_result WS frame, which dashboard.js consumed only
	// for an `announce` + list refetch — both already covered by the
	// cron_run_ended frame. The hook + BroadcastCronResult + cronResultMsg
	// were deleted; the announce moved to dashboard.js's cron_run_ended
	// succeeded branch.
	return result, errMsg, true
}

// redactPathsBuilderPool reuses strings.Builder scratch space across
// redactPathsInCronError slow-path invocations. recordResultP0WithSanitised
// is the hot caller (every cron tick + every TriggerNow). Empty / no-path
// fast-path inputs do not touch the pool. R245-PERF-17 / #872.
//
// Note: strings.Builder.Reset zeroes the internal slice header but cannot
// resize it; b.String() still allocates a fresh string from the buffer
// bytes (Go's strings.Builder is value-only by API), so this pool only
// elides the Builder + initial backing-slice alloc, not the final string
// copy. That is sufficient — the final string copy is unavoidable for
// any non-aliasing implementation.
var redactPathsBuilderPool = sync.Pool{
	New: func() any {
		// 512B initial capacity: most cron error messages are small;
		// long ones grow via Builder.Grow inside the call.
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

// redactPathsInCronError strips absolute filesystem paths from a cron
// execution error message before persistence. session.GetOrCreate and
// session.Send produce errors like "session error: workspace …/repo/x:
// permission denied" that would otherwise enumerate the operator's
// filesystem layout to every authenticated dashboard viewer and any
// cron_jobs.json backup reader. We replace both POSIX and Windows-style
// absolute paths with a literal "<path>" placeholder; error classification
// (permission denied, no such file) stays intact because the surrounding
// tokens aren't paths. R61-SEC-8.
//
// The implementation is a token-wise scan rather than a regex to avoid
// pulling a regex compile onto every cron run: recordResultP0WithSanitised
// is invoked on every execution and the regex cost would dominate the
// redaction budget.
//
// SCOPE — UNC paths are out of scope. R239-GO-9.
// Detection covers three forms: POSIX `/abs`, Windows drive `C:\…` /
// `C:/…`, and home-relative `~/`. Microsoft UNC paths (`\\server\share`
// and the rare `//server/share` POSIX-style equivalent that some Windows
// tools emit) are intentionally NOT matched: the leading `\\` would
// require a peek-ahead second byte (`s[i+1]=='\\'`) which the current
// isWin / isPosix branches don't gate, and a leading `//` looks
// indistinguishable from an empty POSIX path token. naozhi runs on
// Linux containers in production — UNC paths cannot appear in the
// underlying CLI's error messages there. WSL or Windows-mount
// deployments may surface UNC strings unredacted; redaction of those
// forms is a future enhancement (would require a new branch matching
// `\\` / `//` followed by a non-`/` non-`\` host segment).
func redactPathsInCronError(s string) string {
	if s == "" {
		return s
	}
	// Hot fast-path: short error-classifier strings ("context deadline
	// exceeded", "dispatcher queue full") with no path-trigger byte never
	// need truncation OR the Builder pool — return them aliased. The 256B
	// cap is a defensive ceiling so an unexpectedly long no-path input
	// still falls through to the byte-cap branch below; common cron error
	// classes fit comfortably under this. R250-PERF-12 / #1115.
	if len(s) <= redactFastPathMaxLen &&
		strings.IndexByte(s, '/') < 0 &&
		strings.IndexByte(s, '\\') < 0 &&
		strings.IndexByte(s, '~') < 0 {
		return s
	}
	// Byte-level cap, but split on a rune boundary — naked s[:maxRedactErrLen]
	// can fall mid-codepoint for multibyte runes (CJK error messages from the
	// CLI), producing invalid UTF-8 that then poisons cron_jobs.json.
	if len(s) > maxRedactErrLen {
		n := textutil.TruncateAtRuneBoundary(s, maxRedactErrLen)
		s = s[:n] + "…"
	}
	// Fast path: if the string contains no POSIX slash, no Windows
	// backslash, and no '~/' tilde-home shorthand, there is nothing
	// path-shaped to redact — skip the Builder allocation and return the
	// input unchanged. recordResult runs on every cron execution, and
	// common error classes ("dispatcher queue full", "session error:
	// context deadline exceeded") have no embedded paths. R62-PERF-3 +
	// R234-SEC-9（~/ 用户目录形态补漏）。
	if strings.IndexByte(s, '/') < 0 && strings.IndexByte(s, '\\') < 0 && strings.IndexByte(s, '~') < 0 {
		return s
	}
	b := redactPathsBuilderPool.Get().(*strings.Builder)
	// Important: strings.Builder.Reset() drops the internal byte slice
	// entirely (sets it to nil), so we must Reset BEFORE Grow on the
	// pooled instance — otherwise the prior call's residual bytes would
	// prefix this call's output. The pool's New() pre-grows to 512B; the
	// first Reset+Grow on a recycled builder reallocates if and only if
	// len(s) exceeds the residual capacity (which is 0 post-Reset, so a
	// fresh alloc happens here). The win is the *Builder header itself
	// (24B) coming from the pool; the backing []byte still allocates per
	// call. b.String() always allocates a fresh string regardless.
	// Even so, eliminating the per-call *Builder header alloc closes the
	// "double alloc" path called out in R245-PERF-17 / #872.
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
	i := 0
	for i < len(s) {
		c := s[i]
		// POSIX absolute path: leading '/' followed by a non-space/non-quote
		// byte. Drive letter path C:\… also counts.
		//
		// R20260527-COR-10 (#1292): the `i+1 < len(s)` guard AND the explicit
		// rejection of '/'-followed-by-' '/'\t'/'\n' are deliberate. They
		// both treat a lone '/' (root dir reference, never a sensitive path)
		// as a literal byte rather than a redact trigger. Cases that fall
		// through unredacted as a result:
		//   - '/' at end-of-string ("error: /") — bare root, no segments.
		//   - '/' followed by whitespace/newline ("error: /\nnext" or
		//     "/ matched") — same: no path segments to leak.
		// These are not security-relevant because the bare root carries no
		// per-host or per-user information. A multi-segment path like
		// "/home/u" still triggers via s[i+1]='h' (non-space).
		isPosix := c == '/' && i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\t' && s[i+1] != '\n'
		isWin := i+2 < len(s) &&
			((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) &&
			s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/')
		// R234-SEC-9: 识别 "~/" 形态的 home-relative 路径，避免泄露用户
		// 目录层级（容器/ssh 错误中常见）。仅在前置位为分隔符或行首时
		// 触发，防止把 "weight ~5kg" 这种文本误伤。
		isTildeHome := c == '~' && i+1 < len(s) && s[i+1] == '/' &&
			(i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' ||
				s[i-1] == '\'' || s[i-1] == '"' || s[i-1] == '`' ||
				s[i-1] == ',' || s[i-1] == ';' || s[i-1] == '(' || s[i-1] == '=')
		if !isPosix && !isWin && !isTildeHome {
			b.WriteByte(c)
			i++
			continue
		}
		// Consume the path until a delimiter that cannot appear in a
		// typical error-embedded path. Stopping at whitespace is the key
		// rule: error messages from the Go standard library spell paths
		// as tokens separated by whitespace ("open /tmp/x: reason"), and
		// the rare legitimate "path with space" in an error string is
		// vanishingly unlikely to survive redaction cleanly anyway. A
		// conservative scan errs on the side of over-redacting.
		j := i
		for j < len(s) {
			cc := s[j]
			if cc == '\n' || cc == ' ' || cc == '\t' || cc == ',' || cc == ';' ||
				cc == '\'' || cc == '"' || cc == '`' {
				break
			}
			if cc == ':' && j+1 < len(s) && (s[j+1] == ' ' || s[j+1] == '\n') {
				// `path: reason` — stop before the ':' so the reason tail
				// survives redaction.
				break
			}
			j++
		}
		b.WriteString("<path>")
		i = j
	}
	return b.String()
}
