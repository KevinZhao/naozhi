// scheduler_jobs.go: cron Job CRUD path.
//
// Contains every public mutation API (AddJob / DeleteJob / PauseJob /
// ResumeJob / UpdateJob / SetJobPrompt / TriggerNow), every list / lookup
// API, schedule-preview helpers, and the robfig-cron entry registration
// (registerJob) that hooks each Job to the scheduler. Split out of
// scheduler.go to give CRUD its own file separate from the run-time hot
// path (scheduler_run.go) and the lifecycle bootstrap (scheduler.go).
//
// No behaviour change. Methods stay on *Scheduler so private fields
// remain accessible without exporting.

package cron

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	robfigcron "github.com/robfig/cron/v3"
)

// AddJob validates, registers, and persists a new cron job.
func (s *Scheduler) AddJob(j *Job) error {
	if err := validateSchedule(j.Schedule); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}
	// Title 长度校验在 scheduler 层兜底，避免绕过 dashboard handler（例如
	// store 直接加载被篡改的 cron_jobs.json）把超长字符串持久化进内存。
	if n := utf8.RuneCountInString(j.Title); n > MaxCronTitleLen {
		return fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}
	// R244-SEC-P2-5 / #889: AddJob is the canonical create path; mirror
	// SetJobPrompt's strict prompt validation so any non-dashboard caller
	// (test, IM op, future API) cannot persist multi-MB / log-injection
	// prompts to cron_jobs.json. Empty prompts are permitted because the
	// dashboard creates jobs in a paused-with-empty-prompt state to be
	// filled in via SetJobPrompt later.
	if j.Prompt != "" {
		if err := ValidatePromptStrict(j.Prompt); err != nil {
			return err
		}
	}

	// addJobAcquiringLock runs under s.mu (defer Unlock). Splitting the locked
	// section into a helper means every early-return path goes through
	// defer and removes the prior pattern of 4 manual s.mu.Unlock() calls
	// (R228-GO-2): adding a new validation step inside the locked section
	// no longer risks leaking a held mutex on the new error path.
	save, perr := s.addJobAcquiringLock(j)
	if perr != nil {
		// addJobAcquiringLock may surface either a pre-mutation error (capacity
		// rejection — no save returned) or a post-mutation persist error
		// (in-memory insertion already happened). The caller cannot tell
		// the two apart from the error alone, but in either case there
		// is no save() to invoke — addJobAcquiringLock returns nil for save in
		// both branches.
		return perr
	}
	save()
	s.registerStubFromJob(j)
	return nil
}

// addJobAcquiringLock performs the AddJob mutation. Unlike the
// pause/resume/deleteJobLocked siblings (caller-holds-lock convention),
// this helper owns the lifecycle of s.mu — it acquires the lock at entry
// and defers Unlock so every early-return path goes through one place.
// Renamed from addJobLocked (R230C-CR-3 / R228-GO-2): the *Locked suffix in
// this package denotes "caller already holds s.mu", which AddJob's helper
// does not satisfy. The new name keeps the contract obvious at the
// call-site.
func (s *Scheduler) addJobAcquiringLock(j *Job) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.jobs) >= s.maxJobs {
		return nil, fmt.Errorf("max cron jobs reached (%d)", s.maxJobs)
	}

	// Per-chat limit to prevent one chat from exhausting global quota.
	// R237-PERF-5 (#661): O(1) lookup on s.chatJobCount replaces the prior
	// O(maxJobs) linear scan. The scan held s.mu across up to 500 *Job
	// entries on every AddJob — a direct hot-path block on the dashboard
	// 1Hz add path that also stalled TriggerNow / emitRunStarted callers
	// contending on the same mutex. The counter is maintained synchronously
	// in deleteJobLocked / Start so it stays in lock-step with len-by-chat
	// (s.jobs); s.jobs is still the canonical truth, asserted by
	// TestChatJobCount_TracksJobsByChat.
	chatKey := chatJobKey{Platform: j.Platform, ChatID: j.ChatID}
	if s.chatJobCount[chatKey] >= s.maxJobsPerChat {
		return nil, fmt.Errorf("per-chat cron limit reached (%d)", s.maxJobsPerChat)
	}

	id, err := generateID()
	if err != nil {
		// R242-CR-14 (#706): crypto/rand 失败由 generateID 透传，AddJob 是
		// 公共入口（dashboard / IM 创建任务），失败应表现为 add 请求拒绝
		// 而非进程 panic。10 次重试圈仅对 ID 碰撞有意义；rand 整体失效
		// 时所有重试都会复现同一错误，提早 bail 比循环 10 次更诚实。
		return nil, fmt.Errorf("cron: generate job id: %w", err)
	}
	j.ID = id
	// Retry on unlikely ID collision. Bound the loop so a hypothetical
	// degenerate generateID (e.g., a test that injects a deterministic mock
	// or a /dev/urandom failure path) cannot spin AddJob under s.mu and
	// stall the whole scheduler. 10 attempts of 8-byte hex IDs is well
	// beyond any realistic collision rate for maxJobsHardCap=500.
	for i := 0; i < 10; i++ {
		if _, exists := s.jobs[j.ID]; !exists {
			break
		}
		// R238-CR-15: surface every retry rather than only the final failure.
		// A degenerate generateID (mock injection or /dev/urandom stall) would
		// otherwise stay silent until attempt 10 produces the
		// "failed to generate unique job ID" error; logging each collision lets
		// operators see the pattern (same ID repeating) before users hit
		// AddJob errors.
		slog.Warn("cron: job ID collision, retrying", "attempt", i+1, "job_id", j.ID)
		retryID, retryErr := generateID()
		if retryErr != nil {
			// 同上：rand 中途失效，提早返回比继续循环更诚实。
			return nil, fmt.Errorf("cron: regenerate job id (retry %d): %w", i+1, retryErr)
		}
		j.ID = retryID
	}
	if _, exists := s.jobs[j.ID]; exists {
		return nil, fmt.Errorf("cron: failed to generate unique job ID after 10 attempts")
	}
	j.CreatedAt = time.Now()

	if !j.Paused {
		if err := s.registerJob(j); err != nil {
			return nil, err
		}
	}
	s.jobs[j.ID] = j
	// R237-PERF-5 (#661): increment the per-chat counter synchronously
	// with s.jobs so the next addJobAcquiringLock observes the up-to-date
	// count without re-scanning. deleteJobLocked is the paired decrement;
	// the rollback path (s.deleteJobLocked below on persist failure) goes
	// through that helper so the counter unwinds correctly.
	s.chatJobCount[chatKey]++
	save, perr := s.persistJobsLocked()
	if perr != nil {
		// R236-GO-10: persist failed *after* registerJob + map insertion.
		// Without rollback, the in-memory state holds an orphan: cron
		// scheduler has the entry, s.jobs has the *Job, but disk has
		// nothing — every tick logs "job not found" then never cleans
		// up because the cron entry stays registered (the dispatcher's
		// debug log path doesn't call s.cron.Remove). Rolling back
		// via deleteJobLocked unwinds the cron entry and map entry
		// under the still-held s.mu, so the persistence gap surfaces
		// as a clean failure to the caller and a fresh AddJob on the
		// same ID is safe. Earlier review note worried about another
		// goroutine observing the entry between registerJob and
		// persist; that window is enclosed by s.mu (the cron
		// dispatcher's tick fans out via runningJobs CAS without
		// re-entering s.mu for lookup, but execute()'s s.jobs[j.ID]
		// read does take s.mu — see executeJob). So the rollback is
		// observationally consistent.
		//
		// R240-GO-1: deleteJobLocked no longer touches the router
		// stub; in this rollback path the stub was never registered
		// (registerStubFromJob runs in AddJob *after* this helper
		// returns and after a successful save), so no router-side
		// cleanup is needed. resetRouterStub on a never-registered
		// key would be a no-op anyway.
		s.deleteJobLocked(j)
		return nil, perr
	}
	return save, nil
}

// PerChatJobCount returns the number of jobs registered against the
// (Platform, ChatID) chat. Backed by s.chatJobCount (R237-PERF-5 / #661):
// O(1) read, lock-free outside the RLock window, vs the historical
// O(N) scan that addJobAcquiringLock used to enforce maxJobsPerChat.
//
// Intended use: dashboard / metrics surfaces that want to render
// "you have N/M cron jobs in this chat" without paying the cost of a
// full ListJobs walk. Returns 0 for a chat with no registered jobs.
//
// Safe on a nil *Scheduler (returns 0) so dashboard renders during
// the bootstrap window before the scheduler is wired do not NPE.
func (s *Scheduler) PerChatJobCount(plat, chatID string) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chatJobCount[chatJobKey{Platform: plat, ChatID: chatID}]
}

// ListJobs returns jobs for a specific chat.
func (s *Scheduler) ListJobs(plat, chatID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// R247-GO-3: pre-allocate so an empty result marshals as `[]` instead of
	// `null` — keeps the JSON wire-format consistent with ListAllJobsWithNextRun
	// and frontend `.length` defenders unaffected. [BREAKING-LOCAL]
	result := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID {
			result = append(result, *j)
		}
	}
	return result
}

// JobWithNextRun pairs a Job snapshot with its next scheduled run time so
// callers rendering lists (dashboard) don't need a second round-trip per job.
type JobWithNextRun struct {
	Job     Job
	NextRun time.Time
}

// ListAllJobsWithNextRun returns every job plus its next scheduled run.
// Lock strategy: snapshot (*Job, entryID) under s.mu.RLock, release s.mu, then
// call s.cron.Entries() without holding s.mu. Calling cron.Entries inside
// s.mu would invert the lock order taken by the cron dispatcher path
// (cron-internal → execute → recordResultP0WithSanitised → s.mu.Lock),
// risking a deadlock.
//
// R236-PERF-11: this used to call s.cron.Entry(id) per job, but
// robfig/cron v3's Entry is implemented as `for _, e := range Entries()
// { if e.ID == id }` and Entries() takes runningMu — so N jobs cost
// N×N entry walks plus N mutex acquisitions on the dispatcher's mutex.
// Building one entryID→Next map up front collapses the cost to O(N) and
// a single mutex acquisition, which matters when the dashboard list API
// polls at 1 Hz with 50 jobs registered.
func (s *Scheduler) ListAllJobsWithNextRun() []JobWithNextRun {
	s.mu.RLock()
	type pair struct {
		job     Job
		entryID robfigcron.EntryID
	}
	pairs := make([]pair, 0, len(s.jobs))
	for _, j := range s.jobs {
		pairs = append(pairs, pair{job: *j, entryID: j.entryID})
	}
	s.mu.RUnlock()

	// Single Entries() snapshot → entryID-keyed map. Allocates one map
	// per call; the alternative — re-walking the slice per pair — is
	// O(N²) and re-acquires runningMu per Entry() call.
	entries := s.cron.Entries()
	nextByID := make(map[robfigcron.EntryID]time.Time, len(entries))
	for _, e := range entries {
		nextByID[e.ID] = e.Next
	}

	result := make([]JobWithNextRun, 0, len(pairs))
	for _, p := range pairs {
		var next time.Time
		if p.entryID != 0 {
			next = nextByID[p.entryID]
		}
		result = append(result, JobWithNextRun{Job: p.job, NextRun: next})
	}
	return result
}

// deleteJobLocked performs the in-memory side effects of removing a job:
// stop the cron entry and drop the map entry.
//
// Caller must hold s.mu.Lock() and pass a non-nil job that exists in
// s.jobs. Intentionally does NOT delete from s.runningJobs: a concurrent
// execute() for this job may still hold the atomic.Bool and be about to
// CAS it back to false; if a fresh AddJob somehow reused the same ID
// (low but non-zero given the hex8 generator), creating a new guard entry
// here could split the CAS gate between two goroutines and permit double
// execution. Retaining the entry is bounded by maxJobsHardCap (one
// *atomic.Bool per historical job) — cheap vs a correctness gap. R219-CR-4.
//
// R240-GO-1: router.Reset MUST NOT be called from inside this function
// because router.Reset → notifyChange callbacks may attempt to acquire
// s.mu, leading to lock-order inversion / recursive write-lock deadlock.
// Callers are responsible for calling resetRouterStub(j.ID) AFTER they
// release s.mu. EnsureStub's godoc already documents the same
// "must-not-hold-s.mu" contract; this function now respects it.
func (s *Scheduler) deleteJobLocked(j *Job) {
	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
	}
	if _, present := s.jobs[j.ID]; present {
		delete(s.jobs, j.ID)
		// R237-PERF-5 (#661): paired decrement for the addJobAcquiringLock
		// increment. Guarded by the s.jobs membership check above so a
		// double-delete (rollback path calling this on a never-inserted
		// job, or a future caller hitting this twice) cannot drive the
		// counter negative — divergence from s.jobs would silently disable
		// the per-chat cap. Drop the entry when count hits zero so the
		// map's working set tracks the live chat set rather than every
		// chat that has ever owned a job.
		key := chatJobKey{Platform: j.Platform, ChatID: j.ChatID}
		if n := s.chatJobCount[key]; n > 1 {
			s.chatJobCount[key] = n - 1
		} else {
			delete(s.chatJobCount, key)
		}
	}
}

// pauseJobLocked transitions a job to Paused state under s.mu. Returns
// ErrJobAlreadyPaused without mutation if the job is already paused so
// the caller can map it to 409 Conflict. R219-CR-4.
func (s *Scheduler) pauseJobLocked(j *Job) error {
	if j.Paused {
		return fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}
	if j.entryID != 0 {
		s.cron.Remove(j.entryID)
		j.entryID = 0
	}
	j.Paused = true
	return nil
}

// resumeJobLocked transitions a paused job back to active under s.mu by
// re-registering the cron entry. Returns ErrJobNotPaused without mutation
// if the job is not paused, or registerJob's error if re-registration
// fails (e.g. schedule no longer parses) — leaving Paused=true so the
// caller can retry. R219-CR-4.
func (s *Scheduler) resumeJobLocked(j *Job) error {
	if !j.Paused {
		return fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}
	if err := s.registerJob(j); err != nil {
		return err
	}
	j.Paused = false
	return nil
}

// withJobByID 是 DeleteJobByID / PauseJobByID / ResumeJobByID 三 dashboard
// 入口的共用执行框架。R247-CR-1：原本三函数 ~120 行重复 closure + 持锁 +
// persist + unlock-then-save 逻辑，本 helper 收口为 3 阶段：
//
//  1. 持 s.mu.Lock 查 id；缺失即返回 ErrJobNotFound 包装错误；
//  2. 调 op(j) 执行业务变更（可返回 op-specific 错误而无 mutation）；
//     op 成功后 persistJobsLocked 拿 save 闭包；
//  3. 释放 s.mu，调 postCleanup(j)（router.Reset / runStore.DeleteJob
//     之类需在锁外的副作用），然后 save() 落盘。
//
// op 在 s.mu.Lock 下执行；postCleanup 在 s.mu 释放后执行。op 返回
// 非 nil 错误时 perr 透传给上层，且 postCleanup 不会被调用。op == nil
// 表示纯删除/查询无业务校验（DeleteJobByID 用此）。postCleanup == nil
// 表示无锁外副作用（Pause/Resume 用此）。
//
// 返回三元组 (*Job, error)：
//   - 找不到：(nil, ErrJobNotFound 包装)；
//   - op 失败：(nil, op 返回的 err)；
//   - persist 失败：(nil, ErrPersistFailed 包装)；postCleanup 已执行。
//   - 成功：(*Job, nil)。
//
// R241-GO-2/3 的"explicit found/ok"语义在此聚合：内部用 found 区分
// 找不到 vs op 失败，调用方不再重复 if j == nil 的歧义判断。
func (s *Scheduler) withJobByID(
	id string,
	op func(j *Job) error,
	postCleanup func(j *Job),
) (*Job, error) {
	var save func()
	var j *Job
	var found bool
	var opErr error
	var perr error
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		var ok bool
		j, ok = s.jobs[id]
		if !ok {
			perr = fmt.Errorf("%w: id %q", ErrJobNotFound, id)
			return
		}
		if op != nil {
			if err := op(j); err != nil {
				opErr = err
				return
			}
		}
		found = true
		save, perr = s.persistJobsLocked()
	}()

	if opErr != nil {
		return nil, opErr
	}
	if !found {
		return nil, perr
	}
	if postCleanup != nil {
		postCleanup(j)
	}
	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	return s.withJobByID(
		id,
		// op：调 deleteJobLocked 移除 in-memory 记录；不返回错误（删除路径无校验）。
		func(j *Job) error {
			s.deleteJobLocked(j)
			return nil
		},
		// postCleanup：锁外做 router.Reset + runStore.DeleteJob.
		// R240-GO-1: router.Reset 移出 deleteJobLocked，避免在 s.mu 下
		// 走 router callback 触发 lock-order inversion。
		// R238-GO-3: deleteJobLocked 已变内存态，runStore 必须清理，否则
		// runs/<jobID>/ 子树会泄漏；persist 失败也要清，故放在 perr 检查前。
		// P1 cron-run-history: 仅删 runs/<jobID>/，不动用户面 jsonl
		// （RFC §2.3 / §4.4）。
		// R242-ARCH-15 (#758): cleanupRunningJobIfIdle reclaims the
		// s.runningJobs entry when the CAS gate is idle, bounding what
		// was previously an unbounded leak (one *runInflight per
		// historical jobID) over long-lived deployments. The idle check
		// preserves the ID-reuse split-CAS guarantee documented on
		// runningJobs.
		func(j *Job) {
			s.resetRouterStub(j.ID)
			if s.runStore != nil {
				s.runStore.DeleteJob(j.ID)
			}
			s.cleanupRunningJobIfIdle(j.ID)
		},
	)
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	return s.withJobByID(id, s.pauseJobLocked, nil)
}

// ResumeJobByID resumes a paused job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	return s.withJobByID(id, s.resumeJobLocked, nil)
}

// JobUpdate captures fields a dashboard user may edit on an existing cron
// job. Only non-nil pointers are applied, so callers can update a single
// field without resending the rest.
type JobUpdate struct {
	Schedule *string
	Prompt   *string
	WorkDir  *string
	// Notify sets Job.Notify when non-nil. nil leaves the field unchanged;
	// pointer-to-true/false writes the explicit tri-state.
	//
	// R227-CONFIG-1: there's no API to reset Job.Notify back to legacy-default
	// (nil) once a value has been set. Callers wanting that effect must
	// either (a) toggle between true and false explicitly (the typical UX
	// path), or (b) edit cron_jobs.json off-line and restart. Promoting
	// JobUpdate.Notify to a tri-state-with-reset enum is a deferred design
	// decision — the wire format would have to grow a fourth state ("clear")
	// and several /api/cron consumers would need migration.
	Notify *bool
	// NotifyPlatform / NotifyChatID behave like Prompt / WorkDir: nil keeps
	// the existing value, a pointer to "" clears it.
	NotifyPlatform *string
	NotifyChatID   *string
	// FreshContext toggles whether each run resets the session before
	// executing. nil leaves existing behavior unchanged.
	FreshContext *bool
	// Title 是人类可读名称。nil 保持原值；pointer 到 "" 会清空
	// （UI 侧回退到 Prompt 首行）。长度由 handler 层先行校验。
	Title *string
	// Backend 是 CLI backend ID（Sprint 6c, docs/rfc/multi-backend.md §9）。
	// nil 保持原值；pointer 到 "" 显式清空，回落到 router default。
	// 字符/长度由 dashboard handler 的 validateCronBackend 先行把关；
	// 未知 backend 不在此处拒绝（router wrapperFor 会 fallback）。
	Backend *string
}

// applyTo writes every non-nil JobUpdate field onto j. R238-ARCH-14
// (#778): the inline `if upd.X != nil { j.X = *upd.X }` ladder used to
// live inside UpdateJob's locked critical section, growing one branch
// per new patchable field. Pulling the dispatch into a method keeps
// UpdateJob's critical section short (one method call instead of a
// 25-line ladder) and gives every patchable field a single edit point —
// new fields land here without touching UpdateJob's body.
//
// Schedule is intentionally NOT applied here. Schedule mutations require
// re-registering the robfig/cron entry under s.mu (with rollback on
// failure) and the helper has no access to *Scheduler. Keeping Schedule
// on the UpdateJob body localises the cron-side coupling and matches
// the issue's "patch model mixes nil-vs-empty" concern only for the
// pure-data fields.
//
// LastSessionID side effect: WorkDir change clears LastSessionID
// because claude JSONL is keyed by cwd. The same caveats from the
// pre-refactor inline comment apply (relies on AddJob/UpdateJob WorkDir
// pre-normalisation; a non-normalised caller risks a spurious clear,
// not data loss).
//
// Caller must hold s.mu (j is the *Job pulled from s.jobs).
func (upd JobUpdate) applyTo(j *Job) {
	if upd.Prompt != nil {
		j.Prompt = *upd.Prompt
	}
	if upd.WorkDir != nil {
		if *upd.WorkDir != j.WorkDir {
			j.LastSessionID = ""
		}
		j.WorkDir = *upd.WorkDir
	}
	if upd.Notify != nil {
		v := *upd.Notify
		j.Notify = &v
	}
	if upd.NotifyPlatform != nil {
		j.NotifyPlatform = *upd.NotifyPlatform
	}
	if upd.NotifyChatID != nil {
		j.NotifyChatID = *upd.NotifyChatID
	}
	if upd.FreshContext != nil {
		j.FreshContext = *upd.FreshContext
	}
	if upd.Title != nil {
		j.Title = *upd.Title
	}
	if upd.Backend != nil {
		j.Backend = *upd.Backend
	}
}

// UpdateJob applies a partial edit to an existing cron job. Schedule changes
// are validated and re-registered atomically (the old robfig entry is
// removed before the new one is installed) so a failed reschedule leaves
// the previous behavior intact. Prompt/WorkDir changes flow through to the
// router stub so the dashboard sidebar reflects the edit immediately.
func (s *Scheduler) UpdateJob(id string, upd JobUpdate) (*Job, error) {
	// Validate schedule first (no lock needed) so we fail fast on bad input.
	if upd.Schedule != nil {
		if *upd.Schedule == "" {
			return nil, fmt.Errorf("schedule must not be empty")
		}
		if err := validateSchedule(*upd.Schedule); err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", *upd.Schedule, err)
		}
	}
	// Validate WorkDir against allowedRoot here (lock-free) so dashboard
	// edits fail fast with a clear error instead of silently persisting a
	// path that execute() will later refuse at runtime. AddJob's creation
	// path applies the same check; UpdateJob previously skipped it.
	if upd.WorkDir != nil && *upd.WorkDir != "" && s.allowedRoot != "" {
		if !workDirUnderRoot(*upd.WorkDir, s.allowedRoot, s.allowedRootResolved) {
			return nil, fmt.Errorf("work_dir outside allowed root")
		}
	}
	if upd.Title != nil {
		if n := utf8.RuneCountInString(*upd.Title); n > MaxCronTitleLen {
			return nil, fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
		}
	}
	// R244-SEC-P2-5 / #889: UpdateJob is a Scheduler-public entry point that
	// historically wrote *upd.Prompt straight into j.Prompt without a size
	// guard. The dashboard PATCH handler already runs validateCronPrompt at
	// the HTTP edge, but any non-dashboard caller (test, CLI utility, future
	// IM op) bypassing that validator would persist a multi-MB / log-injection
	// prompt to cron_jobs.json. Mirror SetJobPrompt's policy so the cap is
	// consistent across all Scheduler write paths. Empty pointer-to-empty is
	// allowed (clears the prompt to the paused-empty initial state); any
	// non-empty value goes through the strict validator.
	if upd.Prompt != nil && *upd.Prompt != "" {
		if err := ValidatePromptStrict(*upd.Prompt); err != nil {
			return nil, err
		}
	}

	// R239-GO-4: critical section uses defer Unlock so any future return
	// path added inside this block stays correctly unlocked. The closure
	// returns (resultSnapshot, persistCallback, error); save() runs
	// post-unlock to keep the global s.mu off the disk write path.
	result, save, err := func() (Job, func(), error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		j, ok := s.jobs[id]
		if !ok {
			return Job{}, nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		}

		// R238-ARCH-14 (#778): non-Schedule fields applied via JobUpdate.applyTo
		// so the locked critical section stays short and adding a new patchable
		// field is a single edit point on the helper. Schedule stays inline
		// because it requires re-registering the robfig/cron entry under s.mu
		// with rollback semantics (helper has no *Scheduler access).
		upd.applyTo(j)

		if upd.Schedule != nil && *upd.Schedule != j.Schedule {
			// R236-QA-08: snapshot the old schedule so we can roll back the
			// in-memory field if registerJob fails. Without this, a failed
			// re-register left j.Schedule mutated to the new value but with
			// j.entryID=0, so the API returned an error to the client while the
			// job had silently disappeared from the cron scheduler. The client
			// would assume the request was a no-op and retry, but the persisted
			// state file (loaded next start) keeps showing the old schedule
			// because persistJobsLocked never ran for this branch — diverging
			// in-memory state from disk.
			//
			// R246-GO-10: extend the rollback to also re-register the OLD
			// schedule on failure so j.entryID is non-zero and the dashboard
			// keeps showing the correct NextRun. Without this, a failed update
			// (rare — robfig/cron rejects unparseable spec, which the API
			// typically pre-validates) silently leaves the job dormant in the
			// scheduler until the next successful UpdateJob / ResumeJob /
			// process restart, even though j.Schedule is restored. The retry
			// uses the original schedule string which we just removed from the
			// cron, so AddFunc accepts it (it parsed previously).
			oldSchedule := j.Schedule
			j.Schedule = *upd.Schedule
			// Re-register with the new schedule unless paused (paused jobs have
			// no live entry; ResumeJob will register with the new schedule).
			if !j.Paused {
				if j.entryID != 0 {
					s.cron.Remove(j.entryID)
					j.entryID = 0
				}
				if err := s.registerJob(j); err != nil {
					// Roll back the in-memory schedule field so subsequent
					// reads (List, persistJobsLocked on a later mutator) keep
					// showing the original schedule.
					j.Schedule = oldSchedule
					// Best-effort re-register the old schedule so NextRun
					// stays populated. If THIS also fails (extremely unlikely
					// — same string just round-tripped through cron.Remove),
					// j.entryID stays 0 and the next ResumeJob / successful
					// UpdateJob will register a fresh entry; we still return
					// the original error so the caller knows the update was
					// rejected.
					if reErr := s.registerJob(j); reErr != nil {
						slog.Error("cron: failed to restore previous schedule after UpdateJob rollback",
							"job_id", j.ID, "schedule", oldSchedule, "err", reErr)
					}
					return Job{}, nil, fmt.Errorf("re-register cron: %w", err)
				}
			}
		}

		save, perr := s.persistJobsLocked()
		// Value-copy while still under lock so the caller sees a stable result
		// even if another goroutine mutates the job right after we unlock.
		return *j, save, perr
	}()
	if err != nil {
		return nil, err
	}
	save()
	// Pass the snapshotted value (via result) to registerStub so a concurrent
	// SetJobPrompt cannot tear the Prompt/WorkDir pointers we read.
	s.registerStubFromJob(&result)
	slog.Info("cron job updated", "job_id", id,
		"schedule_changed", upd.Schedule != nil,
		"prompt_changed", upd.Prompt != nil,
		"workdir_changed", upd.WorkDir != nil,
		"fresh_context_changed", upd.FreshContext != nil)
	return &result, nil
}

// SetJobPrompt updates a job's prompt. If the job was paused with an empty
// prompt (created from dashboard), it also unpauses and registers the schedule.
//
// Both IM (Hub.runTurn / runTurnPassthrough) and dashboard wshub paths land
// here. The dashboard already validates via server.validateCronPrompt at the
// HTTP edge, but the IM path historically only rejected the empty string —
// so a crafted IM payload could persist multi-MB / bidi / log-injection
// bytes into cron_jobs.json. Centralising the policy in
// ValidatePromptStrict keeps IM and dashboard surfaces in lockstep
// (R243-SEC-8 REPEAT-5). Callers should errors.Is(err, ErrInvalidPrompt)
// to distinguish input-validation failures from ErrJobNotFound /
// ErrPersistFailed.
func (s *Scheduler) SetJobPrompt(id, prompt string) error {
	if err := ValidatePromptStrict(prompt); err != nil {
		return err
	}
	// R246-SEC-10: bound prompt size on this dashboard write path. The
	// dashboard handler runs validateCronPrompt (which enforces
	// maxCronPromptBytesDashboard == cron.MaxPromptBytes) before reaching
	// here, but SetJobPrompt is also exposed via Scheduler so any future
	// caller (or a code path that bypasses validateCronPrompt) would write
	// an unbounded prompt to disk and amplify it across LastResult records.
	// Mirror the same cap as cron run prompts.
	if len(prompt) > MaxPromptBytes {
		return fmt.Errorf("prompt too large: %d bytes (cap %d)", len(prompt), MaxPromptBytes)
	}

	s.mu.Lock()

	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Prompt != "" {
		s.mu.Unlock()
		return nil // already has a prompt, no-op
	}

	j.Prompt = prompt
	// R246-CR-247: capture identity fields under lock so the stub refresh
	// below reads stable values even if a concurrent UpdateJob mutates *Job
	// after the IIFE's deferred Unlock fires. Mirrors AddJob / UpdateJob.
	stubWorkDir := j.WorkDir
	stubLastSession := j.LastSessionID
	waspaused := j.Paused
	if j.Paused {
		// Delegate unpause to the shared helper so the registerJob + Paused
		// flag transition stays consistent with PauseJob/ResumeJob/UpdateJob
		// paths. R226-CR-16.
		if err := s.resumeJobLocked(j); err != nil {
			j.Prompt = "" // rollback: Prompt was empty before this call
			s.mu.Unlock()
			return err
		}
	}
	save, perr := s.persistJobsLocked()
	if perr != nil {
		// Rollback in-memory state before releasing the lock so the
		// live view never reflects an un-persisted mutation.
		// pauseJobLocked failure here is best-effort: only logged, never
		// suppresses the original perr returned to the caller. R243-GO-5.
		j.Prompt = ""
		if waspaused && !j.Paused {
			if rbErr := s.pauseJobLocked(j); rbErr != nil && !errors.Is(rbErr, ErrJobAlreadyPaused) {
				slog.Warn("cron rollback after persist failure also failed",
					"job_id", j.ID, "rollback_err", rbErr, "orig_err", perr)
			}
		}
		s.mu.Unlock()
		return perr
	}
	s.mu.Unlock()
	save()
	// R246-CR-247: refresh the router stub so the dashboard sidebar
	// immediately reflects the new prompt. Without this, the stub keeps the
	// empty-prompt state from the initial AddJob until the next executeJob
	// tick rebuilds it.
	s.registerStubByValue(id, stubWorkDir, prompt, stubLastSession)
	slog.Info("cron job prompt set", "job_id", id, "prompt_len", len(prompt))
	return nil
}

// previewLocation returns the timezone the preview helpers should evaluate
// schedules in. Centralised so the nil-Scheduler fallback (tests / dashboard
// bootstrap before scheduler wiring) and the live scheduler path share a
// single decision point. R219-CR-6.
//
//   - nil receiver → UTC (deterministic across machines, matches the godoc
//     contract historically published on the package-level PreviewSchedule)
//   - non-nil receiver with unset location → time.Local (legacy behaviour
//     when location was never configured; preserved to avoid subtle drift
//     in operator-facing tooling)
//   - configured location wins
func (s *Scheduler) previewLocation() *time.Location {
	if s == nil {
		return time.UTC
	}
	if s.location == nil {
		return time.Local
	}
	return s.location
}

// PreviewSchedule validates a schedule expression and returns the next run
// time. Safe to call on a nil *Scheduler — that path computes in UTC for
// tests / dashboard bootstrap before the scheduler is wired. R219-CR-6:
// previously a free-standing cron.PreviewSchedule existed for this nil
// fallback, and operators had to remember which surface to call; folded
// into the method so callers always invoke (*Scheduler).PreviewSchedule.
func (s *Scheduler) PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now().In(s.previewLocation())), nil
}

// PreviewScheduleN returns the next n run times for a schedule expression, in
// the scheduler's configured timezone. Used by the dashboard to preview what
// "接下来会在这些时间运行" looks like before a user commits to a frequency.
// Callers get a validation error on the first Parse failure; n is clamped to
// a sane range by the caller.
//
// Safe to call on a nil *Scheduler — same fallback as PreviewSchedule
// (UTC). R219-CR-6.
func (s *Scheduler) PreviewScheduleN(schedule string, n int) ([]time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 1
	}
	out := make([]time.Time, 0, n)
	t := time.Now().In(s.previewLocation())
	for i := 0; i < n; i++ {
		t = sched.Next(t)
		out = append(out, t)
	}
	return out, nil
}

// Location returns the timezone the scheduler uses to evaluate cron
// expressions, so the dashboard can surface it alongside preview/next-run
// timestamps.
//
// Safe to call on a nil *Scheduler: returns UTC (matches previewLocation's
// nil branch so dashboard preview / Location calls stay in agreement during
// the bootstrap window when scheduler is not yet wired). R219-CR-6.
func (s *Scheduler) Location() *time.Location {
	if s == nil {
		return time.UTC
	}
	if s.location == nil {
		return time.Local
	}
	return s.location
}

// withJobByPrefix is the IM-prefix counterpart to withJobByID. It collapses
// DeleteJob / PauseJob / ResumeJob (R238-ARCH-4 / #743) — three ~25-line
// twins of "lock → findByPrefix → mutate → persist → unlock → side-effect"
// — into a single 3-phase frame. Layout mirrors withJobByID exactly so a
// reader who learns one helper has learned both.
//
//  1. Acquire s.mu, look up by (idPrefix, plat, chatID); a miss surfaces
//     the findByPrefixLocked error verbatim (typically ErrJobNotFound or
//     "ambiguous prefix").
//  2. Run op(j) inside s.mu; an op error skips persist + postCleanup.
//  3. Release s.mu, run postCleanup(j) lock-free (router.Reset /
//     runStore.DeleteJob), then call save() to land the persist.
//
// Lock-order rationale follows withJobByID's: postCleanup must NOT run
// under s.mu because router callbacks may re-take it (notifyChange
// dead-locks otherwise — R240-GO-1). save() runs after postCleanup so a
// persist failure leaves the in-memory + side-effect state already
// committed (matches the pre-refactor semantics that runStore.DeleteJob
// fires even when persist fails — R238-GO-3).
//
// Error precedence (preserved from the originals):
//   - find miss      → (nil, find err)
//   - op error       → (nil, op err)        ; persist + postCleanup skipped
//   - persist error  → (nil, persist err)   ; postCleanup ALREADY ran
//   - success        → (*Job, nil)
func (s *Scheduler) withJobByPrefix(
	idPrefix, plat, chatID string,
	op func(j *Job) error,
	postCleanup func(j *Job),
) (*Job, error) {
	var save func()
	var j *Job
	var findErr, opErr, perr error
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		j, findErr = s.findByPrefixLocked(idPrefix, plat, chatID)
		if findErr != nil {
			return
		}
		if op != nil {
			if err := op(j); err != nil {
				opErr = err
				return
			}
		}
		save, perr = s.persistJobsLocked()
	}()

	if findErr != nil {
		return nil, findErr
	}
	if opErr != nil {
		return nil, opErr
	}
	if postCleanup != nil {
		postCleanup(j)
	}
	if perr != nil {
		return nil, perr
	}
	save()
	return j, nil
}

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	return s.withJobByPrefix(
		idPrefix, plat, chatID,
		func(j *Job) error {
			s.deleteJobLocked(j)
			return nil
		},
		// R240-GO-1: router.Reset must run lock-free — notifyChange may
		// re-enter s.mu. R238-GO-3: runStore.DeleteJob fires even when
		// persist later fails so runs/<jobID>/ doesn't leak on disk
		// while the in-memory record is gone.
		// R242-ARCH-15 (#758): mirror DeleteJobByID's runningJobs reclaim
		// here — the IM-prefix DeleteJob path is the cron alias side of
		// the same lifecycle, so the leak fix has to land at both
		// entry points.
		func(j *Job) {
			s.resetRouterStub(j.ID)
			if s.runStore != nil {
				s.runStore.DeleteJob(j.ID)
			}
			s.cleanupRunningJobIfIdle(j.ID)
		},
	)
}

// PauseJob pauses a job by ID prefix.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	return s.withJobByPrefix(idPrefix, plat, chatID, s.pauseJobLocked, nil)
}

// ResumeJob resumes a paused job by ID prefix.
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	return s.withJobByPrefix(idPrefix, plat, chatID, s.resumeJobLocked, nil)
}

// NextRun returns the next scheduled run time for a job. R247-GO-9
// [REPEAT-2]: the prior implementation read j.entryID under s.mu.RLock
// then released the lock before calling s.cron.Entry(entryID). A
// concurrent UpdateJob path (which Remove+AddFunc the entry under s.mu)
// could race in that window and return the cron-library zero-value
// Entry{} (Next == time.Time{}) for what is in fact a still-scheduled
// job. Same root cause as R246-GO-1 on TriggerNow's entry read.
//
// Hold s.mu.RLock across both the entryID load AND the cron.Entry call
// so the entry the caller asked about cannot be removed mid-read.
// robfig/cron.Cron.Entry takes its own internal lock — there is no
// lock-order conflict with s.mu (cron's locks never call back into
// scheduler code), so the cross-call hold is safe. The cost is one
// extra contended RLock window per dashboard 1Hz poll, dwarfed by
// the s.cron.Entry sort+scan it wraps.
//
// R238-ARCH-17 (#784): entryID is an unexported runtime-only field that
// is zero-valued on any *Job that did not flow through this Scheduler's
// AddJob / loadJobs path (e.g. a test fixture, a deserialised snapshot,
// or a cross-package caller that passed json.Unmarshal output). The
// previous implementation silently returned time.Time{} in that case,
// which the dashboard / IM reply layer renders as "01/01 00:00" — a
// misleading "unknown next run" that looks like a real schedule. When
// j.entryID is zero, fall back to looking up the live *Job by j.ID in
// s.jobs and reading its entryID; the on-record entryID is the source
// of truth, and a non-existent jobID yields a true zero return.
func (s *Scheduler) NextRun(j *Job) time.Time {
	if j == nil {
		return time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entryID := j.entryID
	if entryID == 0 && j.ID != "" {
		if live, ok := s.jobs[j.ID]; ok {
			entryID = live.entryID
		}
	}
	if entryID == 0 {
		return time.Time{}
	}
	entry := s.cron.Entry(entryID)
	return entry.Next
}

// cronEntryGone reports whether the robfig/cron Entry identified by id
// has been removed (or never existed). robfig/cron's Entry(id) returns a
// zero Entry struct when the entry is unknown, distinguishable by
// WrappedJob == nil — but consumers that test that field directly leak
// the lib's internal struct shape into business code. This helper is the
// single point at which scheduler code touches robfig/cron's removed-entry
// sentinel; any future lib bump that changes the sentinel (or replaces
// Entry() with HasEntry / Lookup-style API) lands here once.
//
// Caller must hold s.mu.RLock or s.mu.Lock — concurrent DeleteJob calls
// s.cron.Remove under s.mu.Lock, so reading the entry without a scheduler
// lock can race with removal. The current caller (TriggerNow) already
// holds the lock for its own snapshotting reasons; the helper does not
// re-acquire so it can be used inside an existing lock window without
// lock-order surprises.
//
// R242-ARCH-29 (#774).
func (s *Scheduler) cronEntryGone(id robfigcron.EntryID) bool {
	if id == 0 {
		return true
	}
	return s.cron.Entry(id).WrappedJob == nil
}

// TriggerNow manually executes a job by ID in a new goroutine (for debugging/dashboard).
// Returns an error if the job is not found, paused, or has no prompt.
func (s *Scheduler) TriggerNow(id string) error {
	s.mu.RLock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Paused {
		s.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobPaused, id)
	}
	if j.Prompt == "" {
		s.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobNoPrompt, id)
	}
	entryID := j.entryID
	jobID := j.ID
	// Register the trigger goroutine with triggerWG before releasing s.mu.
	// This prevents a Stop() on another goroutine from observing triggerWG as
	// empty and returning before our goroutine starts. We pair Add(1) here
	// with a Done() in each goroutine body below; if we bail out before
	// spawning (concurrent delete), we Done() the counter inline.
	s.triggerWG.Add(1)

	// R250-GO-2: hold s.mu.RLock across s.cron.Entry(entryID) and the
	// WrappedJob nil check so a concurrent DeleteJob (which calls
	// s.cron.Remove under s.mu.Lock) cannot observe entryID-in-flight
	// while we're mid-lookup. NextRun (above) already uses the same
	// cross-lock pattern; cron's internal lock cannot call back into
	// scheduler code, so cross-lock holding is safe.
	if entryID != 0 {
		// TriggerNow 不再通过 cron chain 的 WrappedJob.Run()——因为我们要跳过
		// jitter（用户显式 "run now" 期望立刻跑）。改为直接 executeOpt(..., true)。
		// 去 chain 后失去的保护：
		//   1) SkipIfStillRunning —— executeOpt 内部的 jobRunningGuard CAS
		//      同样拒绝重叠，等效覆盖。
		//   2) Recover（panic） —— execute 自身走 session.Send，session 层
		//      panic 已经被上层 recover；即便有残留 panic 也只影响此 goroutine，
		//      不会污染 robfig/cron 调度器。
		// 但必须保留"entry 已被并发 DeleteJob 清掉"的分支：此时 cron.Entry()
		// 的 WrappedJob 为 nil，我们应该把这当作"entry gone"静默退出，不再
		// 走 executeOpt（可能引用已被清理的 session router / job 指针）。
		// 相关测试：TestTriggerNow_EntryGoneReleasesWG（trigger_now_wg_done_test.go）。
		// R192-CRON-B: cron-v2-polish §3.2 jitter。
		entry := s.cron.Entry(entryID)
		entryGone := entry.WrappedJob == nil
		s.mu.RUnlock()
		if entryGone {
			go func() {
				defer s.triggerWG.Done()
				slog.Debug("TriggerNow: cron entry gone (concurrent delete?)", "job_id", id, "entry_id", entryID)
			}()
		} else {
			go func() {
				defer s.triggerWG.Done()
				s.executeIfNotDeletedOrPaused(jobID)
			}()
		}
	} else {
		s.mu.RUnlock()
		go func() {
			defer s.triggerWG.Done()
			s.executeIfNotDeletedOrPaused(jobID)
		}()
	}
	return nil
}

// registerJob registers a job with the robfig/cron scheduler.
//
// The closure captures the job's ID rather than the *Job pointer: if the
// job is removed and re-added (UpdateJob path) while the scheduler goroutine
// holds an old entry, we want the next tick to resolve the currently-registered
// job rather than fire against a stale pointer whose fields may have diverged
// from the user's intent.
//
// R247-CR-10: tick-dispatch closure routes through executeJobIDIfLive
// shared with TriggerNow's executeIfNotDeletedOrPaused, so the
// deleted/paused pre-flight gate stays in one place. A Pause that lands
// between cron-tick dispatch and our re-lock is honored — PauseJobByID
// removes the entry via cron.Remove(), so normally this tick wouldn't
// fire, but robfig/cron may already be mid-dispatch when Remove runs,
// yielding exactly this race.
func (s *Scheduler) registerJob(j *Job) error {
	jobID := j.ID
	// R247-CR-10 / R250-CR-1 (#1134): route the scheduled tick through
	// executeJobIDIfLive so the {RLock → exists/paused → executeOpt}
	// sequence shared with TriggerNow lives in one place. The closure
	// captures jobID (not *Job) so an UpdateJob remove+re-add between
	// tick dispatch and re-lock resolves to the freshest pointer. The
	// "tick fired for job paused concurrently" race (PauseJobByID's
	// cron.Remove vs robfig mid-dispatch) is honoured by
	// executeJobIDIfLive's paused branch — same Debug log, same skip.
	// The previous godoc named "executeIfReadyOpt", a rename casualty
	// from R247-CR-10 that no helper actually carries.
	//
	// R246-ARCH-9 (#785): the AddFunc closure is constructed via
	// (*Scheduler).newCronTickCallback so the dispatch-boundary contract
	// (jobID-only capture, no *Job pointer leak, single executeJobIDIfLive
	// call site) is documented and pinned in one place. The Scheduler's
	// stopCtx struct field still owns the lifecycle context — robfig/cron's
	// AddFunc takes a func() with no ctx parameter so the field cannot be
	// eliminated entirely until the upstream API grows ctx-aware Schedule.
	// Wrapping the closure here at least makes the dispatch boundary
	// explicit for future ctx-flow refactors (e.g. lifting executeOpt's
	// downstream s.stopCtx reads to receive ctx as a parameter).
	entryID, err := s.cron.AddFunc(j.Schedule, s.newCronTickCallback(jobID))
	if err != nil {
		return fmt.Errorf("register cron: %w", err)
	}
	j.entryID = entryID
	// R242-PERF-2 (#664): cache the schedule period now so the per-tick
	// applyJitterSched fast-path can read it instead of running 2× sched.Next
	// on every fire. Period only depends on Schedule; UpdateJob's Schedule
	// branch (line ~627) calls registerJob again after Remove, so the cache
	// stays in lockstep with the live entry. Zero on parse failure leaves
	// callers on the existing fallback (jitterSleep with period<=0 uses the
	// full jitterMax window).
	if sched := s.cron.Entry(entryID).Schedule; sched != nil {
		j.cachedPeriod = schedulePeriodFromSched(sched, time.Now())
	} else {
		j.cachedPeriod = 0
	}
	return nil
}

// newCronTickCallback returns the func() closure registered with
// robfig/cron's AddFunc for jobID. R246-ARCH-9 (#785): isolating the
// dispatch boundary in one factory makes three contracts explicit:
//
//  1. The closure captures jobID by value, NOT a *Job pointer. An
//     UpdateJob remove+re-add between tick dispatch and re-lock must
//     resolve to the freshest entry, which executeJobIDIfLive does by
//     re-reading s.jobs[jobID] under RLock. Capturing *Job here would
//     leak a stale pointer past the next UpdateJob.
//
//  2. The closure delegates to executeJobIDIfLive — never calls
//     executeOpt directly — so the deleted/paused pre-flight gate
//     stays shared with TriggerNow's path. R247-CR-10 / R250-CR-1
//     (#1134) is the historical anchor.
//
//  3. The viaTriggerNow=false / logSubject="cron" pair is fixed at
//     the dispatch boundary; future tick-dispatch fan-outs (e.g. a
//     "missed-schedule replay" trigger) must mint their own factory
//     to keep the trigger-source label in lockstep with the dispatch
//     path.
//
// Lifting this from an inline closure also gives a stable structural
// anchor for future ctx-aware AddFunc shims if robfig/cron grows a
// ctx parameter — the wrapper signature is the single place a ctx
// argument would land. Until then s.stopCtx remains a Scheduler
// struct field (see scheduler.go godoc on the field's anti-pattern
// rationale: robfig/cron callbacks have no ctx parameter slot).
func (s *Scheduler) newCronTickCallback(jobID string) func() {
	return func() {
		s.executeJobIDIfLive(jobID, false /* viaTriggerNow */, "cron")
	}
}

// findByPrefixLocked finds a job by ID prefix scoped to a specific chat.
//
// LOCK: caller MUST hold s.mu (read or write). The body iterates s.jobs
// directly without taking the mutex; every in-tree caller (DeleteJob /
// PauseJob / ResumeJob) already holds s.mu.Lock() across the find +
// mutate + persist window, so the *Locked suffix is a documentation
// contract, not a behaviour change. Renamed under R20260526-GO-002 to
// match the package convention (deleteJobLocked / pauseJobLocked /
// persistJobsLocked / …) so future callers see the locking requirement
// without grepping the call graph.
func (s *Scheduler) findByPrefixLocked(idPrefix, plat, chatID string) (*Job, error) {
	var matches []*Job
	for _, j := range s.jobs {
		if j.Platform == plat && j.ChatID == chatID && strings.HasPrefix(j.ID, idPrefix) {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("%w: prefix %q matches %s", ErrAmbiguousPrefix, idPrefix, strings.Join(ids, ", "))
	}
}
