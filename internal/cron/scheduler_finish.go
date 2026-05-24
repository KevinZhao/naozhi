// scheduler_finish.go: terminal hooks for every cron execution path
// (write side) plus run-history queries the dashboard reads (read side).
//
// Centralising the finish path here keeps the seven branches of executeOpt
// converging on a single struct literal (finishArgs) and lets the dashboard
// query API (CurrentRun / ListRuns / RecentRuns / GetRun) live next to the
// writers that produce the records — when the schema of CronRun changes,
// readers and writers move together. No behaviour change. Methods stay on
// *Scheduler so the s.mu / s.jobs / s.runStore / s.onExecute /
// s.runningJobs fields remain accessible without exporting.

package cron

import (
	"io/fs"
	"log/slog"
	"strings"
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
		persistedResult, persistedErrMsg, jobPersistOK = s.recordResultP0WithSanitised(a.job, a.result, a.errMsg, a.sessionID, a.errClass, a.state)
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
			Prompt:      a.prompt,
			WorkDir:     a.workDir,
			Fresh:       a.fresh,
			Result:      persistedResult,
			ResultBytes: len(persistedResult),
			ErrorClass:  a.errClass,
			ErrorMsg:    persistedErrMsg,
		})
	}

	// Broadcast last so server-side hub locks aren't held while we hold s.mu.
	// ErrorMsg uses persistedErrMsg (post-redact, post-sanitise) — see the
	// SECURITY note above for why a.errMsg is never used here.
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
// CAS-rejected execution attempt. Despite the "Skipped" terminology, this
// function emits BOTH a RunStarted event AND drives finishRun (which emits
// RunEnded), so subscribers see the same started→ended pair as a normal
// run; the state field carries RunStateSkipped + ErrClassOverlapSkipped so
// dashboards can render it as a no-op pill instead of a real run timeline.
// CronRunStartedTotal (via emitRunStarted) + the per-state metric (via
// finishRun) both bump.
//
// This dual-event emission is intentional: it keeps the runs/<id> dashboard
// drawer renderable and prevents subscriber state machines from missing
// the "started" anchor when a manual TriggerNow collides with an
// in-flight run. R233B-CR-2.
//
// The CAS gate trips before any inflight metadata is populated, so we
// synthesise a RunID + StartedAt locally — finishRun's skipPersist=true
// short-circuit avoids writing the synthetic run to disk.
func (s *Scheduler) emitOverlapSkipped(j *Job, viaTriggerNow bool) {
	runID := generateRunID()
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
		state: RunStateSkipped, errClass: ErrClassOverlapSkipped,
		errMsg: "previous run still in flight", skipPersist: true,
	})
}

// recordResultP0WithSanitised persists the terminal result (LastResult /
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
func (s *Scheduler) recordResultP0WithSanitised(j *Job, result, errMsg, sessionID string, errClass ErrorClass, state RunState) (string, string, bool) {
	// truncateWithSuffix (limits.go) is the single source of truth for the
	// rune-trim + …[truncated] suffix; both this path and sanitiseRunResult
	// must produce byte-identical output so the skipPersist branch of
	// finishRun and the disk record never disagree on visible content.
	// R234-CR-1 consolidated three open-coded copies into the helper.
	result = truncateWithSuffix(result, maxStoredResultRunes)
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
	prev := struct {
		LastRunAt      time.Time
		LastResult     string
		LastError      string
		LastErrorClass ErrorClass
		LastSessionID  string
		Counters       JobRunCounters
	}{j.LastRunAt, j.LastResult, j.LastError, j.LastErrorClass, j.LastSessionID, j.RunCounters}

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
		j.LastRunAt = prev.LastRunAt
		j.LastResult = prev.LastResult
		j.LastError = prev.LastError
		j.LastErrorClass = prev.LastErrorClass
		j.LastSessionID = prev.LastSessionID
		j.RunCounters = prev.Counters
		s.mu.Unlock()
		slog.Warn("cron: recordResultP0 persist failed; in-memory result reverted",
			"job_id", j.ID, "err", perr)
		return result, errMsg, false
	}
	// Snapshot j.ID before releasing s.mu so the post-unlock onExecute
	// callback does not depend on the implicit "Job.ID is immutable across
	// concurrent DeleteJob" contract — that contract holds today (DeleteJob
	// removes the entry from s.jobs but never mutates *Job in place), but
	// pinning the value here makes future refactors safer. R235-GO-1.
	jobID := j.ID
	s.mu.Unlock()

	save()
	if fn := s.onExecute.Load(); fn != nil {
		(*fn)(jobID, result, errMsg)
	}
	return result, errMsg, true
}

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
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// POSIX absolute path: leading '/' followed by a non-space/non-quote
		// byte. Drive letter path C:\… also counts.
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
