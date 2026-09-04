package cron

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// registerStubByValue creates (or refreshes) a router stub for the job so it
// appears in the dashboard workspace list. Returns false when no router is
// wired (the bool flows back into EnsureStub so a no-op stays visible, #491).
// Callers must not hold s.mu — RegisterCronStubWithChain re-enters router
// state. Takes values, not *Job, so a locked caller cannot leak a pointer that
// a concurrent UpdateJob mutates; snapshot fields first.
//
// 当 lastSessionID 非空（最近一次成功执行的 session_id），会作为单元素
// chain 传给 stub，这样 dashboard 点击 cron 侧边栏时能按该 ID 从 claude
// 项目目录找到 JSONL 历史；否则 fresh_context=true 的任务每次 Reset 都会清空 chain。
func (s *Scheduler) registerStubByValue(id, workDir, prompt, lastSessionID string) bool {
	if s.router == nil {
		s.routerNilOnce.Do(func() {
			slog.Error("cron: registerStubByValue called without a router; dashboard sidebar will be empty for this scheduler — wireup bug or missing SchedulerDeps.Router?",
				"job_id", id)
		})
		return false
	}
	var chain []string
	if lastSessionID != "" {
		chain = []string{lastSessionID}
	}
	s.router.RegisterCronStubWithChain(sessionkey.CronKey(id), workDir, prompt, chain)
	return true
}

// registerStubFromJob 是 registerStubByValue 的便捷包装，对未持锁、且对
// *Job 字段稳定性已有把握（如 AddJob 后立刻调）的调用方简化字面。
func (s *Scheduler) registerStubFromJob(j *Job) bool {
	return s.registerStubByValue(j.ID, j.WorkDir, j.Prompt, j.LastSessionID)
}

// EnsureStub lazily (re-)registers a dashboard stub session for the given
// key (format "cron:<jobID>"). Returns true when the matching job still
// exists and a stub is now registered; false when the key is malformed, not a
// cron key, or the job is gone.
//
// The sidebar "×" routes through router.Remove and deletes the stub; until the
// next tick rebuilds it, clicking the task card in the Cron panel would hit
// "session not found". This is the idempotent recovery hook wired into
// handleSubscribe and /api/sessions/events. Safe on a nil *Scheduler: a
// typed-nil stored in a CronView interface bypasses `h.scheduler != nil` guards.
func (s *Scheduler) EnsureStub(key string) bool {
	if s == nil {
		return false
	}
	if !sessionkey.IsCronKey(key) {
		return false
	}
	id := key[len(sessionkey.CronKeyPrefix):]
	if id == "" {
		return false
	}
	// Snapshot workDir/prompt under RLock, release before reaching into
	// router: RegisterCronStubWithChain calls notifyChange which fans out to
	// hub broadcasters, and holding s.mu across that path risks lock-order
	// inversion with the cron dispatcher (see ListAllJobsWithNextRun).
	s.mu.RLock()
	j, ok := s.jobs[id]
	var workDir, prompt, lastSessionID string
	if ok {
		workDir = j.WorkDir
		prompt = j.Prompt
		lastSessionID = j.LastSessionID
	}
	s.mu.RUnlock()
	if !ok {
		return false
	}
	return s.registerStubByValue(id, workDir, prompt, lastSessionID)
}

// resetRouterStub is the deferred router-side cleanup that pairs with
// deleteJobLocked. Caller MUST NOT hold s.mu — router.Reset re-enters router
// state and its notifyChange callback may take s.mu. Safe on a nil router and
// on a nil receiver (partial test fixtures drive deletion paths).
func (s *Scheduler) resetRouterStub(jobID string) {
	if s == nil {
		return
	}
	if s.router == nil {
		return
	}
	s.router.Reset(sessionkey.CronKey(jobID))
}
