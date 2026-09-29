// scheduler_jobs.go: cron Job CRUD path — public mutation APIs, list /
// lookup APIs, and the robfig-cron entry registration (registerJob).
// Run-time hot path lives in scheduler_run.go, lifecycle in scheduler.go.

package cron

import (
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"
)

// AddJob validates, registers, and persists a new cron job.
func (s *Scheduler) AddJob(j *Job) error {
	if err := validateSchedule(j.Schedule, s.previewLocation()); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
	}
	// Title 长度校验在 scheduler 层兜底，避免绕过 dashboard handler（例如
	// store 直接加载被篡改的 cron_jobs.json）把超长字符串持久化进内存。
	if n := utf8.RuneCountInString(j.Title); n > MaxCronTitleLen {
		return fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
	}
	// Mirror SetJobPrompt's strict validation so non-dashboard callers cannot
	// persist multi-MB / log-injection prompts. Empty is allowed: the dashboard
	// creates paused-with-empty-prompt jobs filled in via SetJobPrompt (#889).
	if j.Prompt != "" {
		if err := ValidatePromptStrict(j.Prompt); err != nil {
			return err
		}
	}
	// Defence-in-depth: the caps loadJobs applies on the read path run on the
	// write path too, so no internal caller can persist an oversized WorkDir
	// or log-injection NotifyChatID bytes (#1141).
	if err := validateJobFields(j); err != nil {
		return err
	}

	// Entry-lifecycle writer: hold entryMu for the whole plan → insert → persist
	// → commit → apply span so no concurrent writer can touch this job's entry
	// while it is half-made. Taken before s.tbl.mu per the lock order.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()

	// The table plans the cron entry but does not commit it, so a persist
	// failure leaves no robfig entry to roll back.
	r, err := s.tbl.insert(j, s.maxJobs, s.maxJobsPerChat)
	if err != nil {
		return err
	}
	// Commit outside s.tbl.mu (the robfig rendezvous), then write the id back. In
	// the window between insert and apply the job is visible with entryID 0 —
	// the same transient UpdateJob's schedule swap already shows — and entryMu
	// keeps every other entry writer out of it.
	if r.plan != nil {
		s.commitAndApplyCronEntry(*r.plan)
	}
	s.save(r.snap)
	s.registerStubByValue(r.stub.id, r.stub.workDir, r.stub.prompt, r.stub.lastSessionID)
	return nil
}

// deleteJobPostCleanup runs the lock-free side effects that must follow
// deleteLocked, shared by DeleteJobByID and DeleteJob. Caller MUST NOT
// hold s.tbl.mu:
//   - cron Remove of removeEntryID (0 = no entry; Remove(0) is a no-op) keeps
//     the unbuffered c.remove send off the s.tbl.mu write hold (#1810);
//   - resetRouterStub: router.Reset callbacks may re-enter s.tbl.mu;
//   - runStore.DeleteJob fires even when persist failed so runs/<jobID>/ does
//     not leak once the in-memory record is gone;
//   - cleanupRunningJobIfIdle bounds the per-jobID *runInflight leak (#758).
func (s *Scheduler) deleteJobPostCleanup(jobID string, removeEntryID cronEntryID) {
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	s.resetRouterStub(jobID)
	// agentcore §6.2: a sandbox job deleted mid-run must Stop its microVM.
	// Best-effort + idempotent; runs before deleteJobRuns so the pending file
	// is resolved before the runs/ tree is swept.
	s.stopSandboxRunsForJob(jobID)
	s.deleteJobRuns(jobID)
	s.gate.cleanupRunningJobIfIdle(jobID)
}

// JobUpdate captures fields a dashboard user may edit on an existing cron
// job. Only non-nil pointers are applied, so callers can update a single
// field without resending the rest.
type JobUpdate struct {
	Schedule *string
	Prompt   *string
	WorkDir  *string
	// Notify sets Job.Notify when non-nil; nil leaves it unchanged. Use
	// NotifyClear to reset back to legacy-default nil — a separate flag keeps
	// the wire format source-compatible (#958).
	Notify *bool
	// NotifyClear (pointer-to-true) resets Job.Notify to nil (inherit the
	// scheduler-wide policy). Applied AFTER Notify so an explicit clear wins
	// if a caller sends both (#958).
	NotifyClear *bool
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
	// Backend 是 CLI backend ID（docs/rfc/multi-backend.md §9）。nil 保持原值；
	// pointer 到 "" 显式清空，回落到 router default。字符/长度由 dashboard
	// handler 先行把关；未知 backend 不在此处拒绝（router wrapperFor 会 fallback）。
	Backend *string
	// Placement 是运行位置（agentcore-cloud-sandbox RFC §4.2）。nil 保持
	// 原值；pointer 到 "" 或 "local" 回落本机；"sandbox" 走 AgentCore
	// run-once。validatePlacement 在 UpdateJob 入口拒绝未知值。
	Placement *string
	// SideEffects 切换"任务有外部副作用"声明（agentcore §6.2 双跑围栏）。
	// nil 保持原值；pointer 到 true/false 写显式三态。无 clear 语义——
	// 与 Placement 一样属"运行属性"，不像 Notify 需要回 legacy-default。
	SideEffects *bool
}

// applyTo writes every non-nil JobUpdate field onto j. Caller must hold s.tbl.mu
// (j is the *Job from s.tbl.jobs). Schedule is intentionally NOT applied here:
// schedule changes re-register the robfig/cron entry with rollback, which
// needs *Scheduler, so they stay in UpdateJob's body. A WorkDir change clears
// LastSessionID because claude JSONL is keyed by cwd (relies on callers
// pre-normalising WorkDir; a non-normalised caller risks a spurious clear).
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
	// Applied after Notify so an explicit clear wins if both are sent.
	if upd.NotifyClear != nil && *upd.NotifyClear {
		j.Notify = nil
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
	if upd.Placement != nil {
		j.Placement = *upd.Placement
	}
	if upd.SideEffects != nil {
		v := *upd.SideEffects
		j.SideEffects = &v
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
		if err := validateSchedule(*upd.Schedule, s.previewLocation()); err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", *upd.Schedule, err)
		}
		// A schedule change swaps this job's robfig entry, and that swap spans the
		// window between the table edit and the commit, where s.tbl.mu is released
		// on purpose. entryMu is what keeps two such swaps from each registering an
		// entry and leaving the job firing on the union of two schedules — see
		// entry_registration.go.
		//
		// Taken only when a schedule change is REQUESTED, so prompt/workdir edits
		// never touch it, and taken before s.tbl.mu to honour the lock order. Held to
		// function exit rather than to the end of the re-register block: the
		// rollback paths in between also write the entry id, and a schedule change
		// is a dashboard edit, not a hot path.
		s.entryMu.Lock()
		defer s.entryMu.Unlock()
	}
	// Lock-free WorkDir check so dashboard edits fail fast instead of
	// persisting a path execute() will refuse at runtime.
	if upd.WorkDir != nil {
		v := *upd.WorkDir
		if len(v) > MaxWorkDirLen {
			return nil, fmt.Errorf("cron: work_dir too long: %d bytes > %d cap", len(v), MaxWorkDirLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: work_dir contains invalid bytes")
		}
		if v != "" && s.allowedRoot != "" {
			if !workDirUnderRoot(v, s.allowedRoot, s.allowedRootResolved) {
				return nil, fmt.Errorf("work_dir outside allowed root")
			}
		}
	}
	if upd.Title != nil {
		if n := utf8.RuneCountInString(*upd.Title); n > MaxCronTitleLen {
			return nil, fmt.Errorf("title too long: %d runes > %d cap", n, MaxCronTitleLen)
		}
	}
	// Mirror SetJobPrompt's strict policy so non-dashboard callers cannot
	// persist multi-MB / log-injection prompts (#889). Pointer-to-empty is
	// allowed (clears to the paused-empty initial state).
	if upd.Prompt != nil && *upd.Prompt != "" {
		if err := ValidatePromptStrict(*upd.Prompt); err != nil {
			return nil, err
		}
	}
	// Mirror validateJobFields' length + UTF-8 + containsCronUnsafe guards so
	// non-dashboard callers cannot write oversized / log-injection bytes.
	if upd.NotifyPlatform != nil {
		v := *upd.NotifyPlatform
		if len(v) > MaxNotifyTargetLen {
			return nil, fmt.Errorf("cron: notify_platform too long: %d bytes > %d cap", len(v), MaxNotifyTargetLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: notify_platform contains invalid bytes")
		}
	}
	if upd.NotifyChatID != nil {
		v := *upd.NotifyChatID
		if len(v) > MaxNotifyTargetLen {
			return nil, fmt.Errorf("cron: notify_chat_id too long: %d bytes > %d cap", len(v), MaxNotifyTargetLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: notify_chat_id contains invalid bytes")
		}
	}
	// Same guards as validateJobFields for Backend.
	if upd.Backend != nil {
		v := *upd.Backend
		if len(v) > MaxBackendLen {
			return nil, fmt.Errorf("cron: backend too long: %d bytes > %d cap", len(v), MaxBackendLen)
		}
		if !utf8.ValidString(v) || containsCronUnsafe(v) {
			return nil, fmt.Errorf("cron: backend contains invalid characters")
		}
	}
	if upd.Placement != nil {
		if err := validatePlacement(*upd.Placement); err != nil {
			return nil, fmt.Errorf("cron: %w", err)
		}
	}

	// The table applies the edit and persists it; the robfig Remove and commit
	// run here with s.tbl.mu released. They rendezvous with the run loop — a
	// LATENCY problem, not a deadlock one (run() never takes runningMu) — and
	// holding the registry lock across that parks every reader behind it.
	// entryMu, held since the top for schedule changes, keeps every other entry
	// writer out of the entryID=0 window in between.
	r, err := s.tbl.update(id, upd)
	if err != nil {
		return nil, err
	}
	result := r.job
	if rs := r.resched; rs != nil {
		if rs.removeEntry != 0 {
			s.cron.Remove(rs.removeEntry)
		}
		// Plan is pure (no lock, no robfig) and cannot fail here in practice:
		// validateSchedule at the top of UpdateJob uses the same parser. The
		// branch below is defence for the day those two drift apart, and it has
		// to exist because by now the old entry is gone — failing silently would
		// leave a job that never fires again.
		if p, planErr := planCronEntry(id, rs.newSchedule, time.Now()); planErr == nil {
			s.commitAndApplyCronEntry(p)
		} else {
			s.revertReschedule(id, rs.oldSchedule)
			return nil, fmt.Errorf("re-register cron: %w", planErr)
		}
	}
	// Refresh LastSessionID from the live job: result was snapshotted before
	// the entry commit and a concurrent recordTerminalResult may have written
	// a newer session id, which would anchor the sidebar stub on a stale one.
	if r.resched != nil {
		if live, ok := s.tbl.lastSessionID(id); ok {
			result.LastSessionID = live
		}
	}
	s.save(r.snap)
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

// revertReschedule rolls job id back to oldSchedule after its new schedule
// failed to plan, best-effort restores the old entry, and persists the
// rolled-back state so disk agrees. If even the old schedule no longer plans
// the job cannot fire, so it is marked Paused and the dashboard shows the
// degraded state instead of an active job with no entry.
func (s *Scheduler) revertReschedule(id, oldSchedule string) {
	oldPlan, oldErr := planCronEntry(id, oldSchedule, time.Now())
	if oldErr != nil {
		slog.Error("cron: failed to restore previous schedule after UpdateJob rollback",
			"job_id", id, "schedule", oldSchedule, "err", oldErr)
	}
	snap, found, err := s.tbl.revertSchedule(id, oldSchedule, oldErr != nil)
	if found && err != nil {
		slog.Error("cron: re-persist after UpdateJob rollback failed", "job_id", id, "err", err)
	} else if found {
		s.save(snap)
	}
	if oldErr == nil {
		s.commitAndApplyCronEntry(oldPlan)
	}
}

// SetJobPrompt sets a job's FIRST prompt. If the job was paused with an empty
// prompt (created from dashboard), it also unpauses and registers the schedule.
//
// Contract: auto-fill-once, NOT a general update. If the job already has a
// non-empty prompt it returns ErrPromptAlreadySet without mutating (#1503);
// IM auto-save treats it as benign, HTTP callers may map it to 409. Prompt
// changes go through UpdateJob. Both IM and dashboard paths land here, so
// ValidatePromptStrict is enforced centrally; callers errors.Is(err,
// ErrInvalidPrompt) to separate validation failures from ErrJobNotFound /
// ErrPersistFailed.
func (s *Scheduler) SetJobPrompt(id, prompt string) error {
	if err := ValidatePromptStrict(prompt); err != nil {
		return err
	}
	// Bound prompt size here too: SetJobPrompt is exposed via Scheduler, so a
	// caller bypassing the dashboard validator would otherwise write an
	// unbounded prompt to disk and amplify it across LastResult records.
	if len(prompt) > MaxPromptBytes {
		return fmt.Errorf("prompt too large: %d bytes (cap %d)", len(prompt), MaxPromptBytes)
	}

	// Entry-lifecycle writer: the empty-prompt fill may resume the job, which
	// swaps its cron entry. Taken unconditionally (before s.tbl.mu, per the lock
	// order) rather than peeking at Paused first — the peek would be its own
	// race, and this path is a dashboard edit with nothing to contend for.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()

	r, err := s.tbl.fillPrompt(id, prompt)
	if err != nil {
		return err
	}
	// Commit the resume outside s.tbl.mu; entryMu (held since entry) fences the
	// entryID=0 window against every other entry writer.
	if r.plan != nil {
		s.commitAndApplyCronEntry(*r.plan)
	}
	s.save(r.snap)
	// Refresh the router stub so the sidebar reflects the new prompt now
	// rather than at the next executeJob tick.
	s.registerStubByValue(r.stub.id, r.stub.workDir, r.stub.prompt, r.stub.lastSessionID)
	slog.Info("cron job prompt set", "job_id", id, "prompt_len", len(prompt))
	return nil
}

// NextRun returns the next scheduled run time for a job. entryID is resolved
// under s.tbl.mu.RLock and s.tbl.mu is released BEFORE s.cron.Entry(): Entry walks
// Entries(), which round-trips the dispatcher's snapshot channel, so holding
// s.tbl.mu across it would invert the lock order the cron dispatch path takes
// (cron-internal → execute → recordResult → s.tbl.mu.Lock) — the same discipline
// ListAllJobsWithNextRun follows (#1117).
//
// When j.entryID is zero (a *Job that did not flow through AddJob / loadJobs,
// e.g. a deserialised snapshot) fall back to the live s.tbl.jobs[j.ID] record so
// the dashboard does not render a misleading "01/01 00:00" (#784).
func (s *Scheduler) NextRun(j *Job) time.Time {
	if j == nil {
		return time.Time{}
	}
	// Resolve entryID under RLock, release, then read cron (see godoc).
	// TriggerNow deliberately keeps its cross-lock: it needs one consistent
	// instant for the entry-gone check against a racing DeleteJob.
	s.tbl.mu.RLock()
	entryID := j.entryID
	if entryID == 0 && j.ID != "" {
		if live, ok := s.tbl.jobs[j.ID]; ok {
			entryID = live.entryID
		}
	}
	s.tbl.mu.RUnlock()
	if entryID == 0 {
		return time.Time{}
	}
	entry := s.cron.Entry(entryID)
	return entry.Next
}

// cronEntryGoneLocked reports whether the robfig/cron Entry identified by id
// has been removed (or never existed). It is the single point where scheduler
// code touches robfig's removed-entry sentinel (zero Entry, WrappedJob == nil),
// so a lib bump that changes the sentinel lands here once (#774).
//
// Caller must hold s.tbl.mu (read or write) so the read cannot race a concurrent
// delete; the helper does not re-acquire, so it is safe inside an existing
// lock window.
func (s *Scheduler) cronEntryGoneLocked(id cronEntryID) bool {
	if id == 0 {
		return true
	}
	return s.cron.Entry(id).WrappedJob == nil
}

// TriggerNow manually executes a job by ID in a new goroutine (for debugging/dashboard).
// Returns an error if the job is not found, paused, or has no prompt.
func (s *Scheduler) TriggerNow(id string) error {
	s.tbl.mu.RLock()
	// Gate triggerWG.Add behind the stopped flag: stopWithCtx sets s.stopped
	// before draining triggerWG, and an in-flight HandleTrigger could otherwise
	// Add(1) from zero concurrently with Wait, violating the WaitGroup contract
	// and letting a trigger goroutine escape the drain barrier (#2012).
	if s.stopped.Load() {
		s.tbl.mu.RUnlock()
		return ErrSchedulerStopped
	}
	j, ok := s.tbl.jobs[id]
	if !ok {
		s.tbl.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobNotFound, id)
	}
	if j.Paused {
		s.tbl.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobPaused, id)
	}
	if j.Prompt == "" {
		s.tbl.mu.RUnlock()
		return fmt.Errorf("%w: id %q", ErrJobNoPrompt, id)
	}
	entryID := j.entryID
	jobID := j.ID
	// Add to triggerWG before releasing s.tbl.mu so a concurrent Stop() cannot see
	// an empty WaitGroup and return before our goroutine starts; paired with
	// the single deferred Done() in the goroutine body.
	s.triggerWG.Add(1)

	// Hold s.tbl.mu.RLock across cron.Entry + the entry-gone check so a racing
	// DeleteJob is observed at one consistent instant (cron's lock never calls
	// back into scheduler code). entryID==0 means paused/unregistered, never
	// "gone". TriggerNow 跳过 cron chain 直接 executeOpt（"run now" 不要 jitter）；
	// 重叠由 jobRunningGuard CAS 覆盖，panic 由 recordTriggerNowPanic recover 覆盖。
	entryGone := entryID != 0 && s.cronEntryGoneLocked(entryID)
	s.tbl.mu.RUnlock()

	go func() {
		defer s.triggerWG.Done()
		if entryGone {
			slog.Debug("TriggerNow: cron entry gone (concurrent delete?)", "job_id", id, "entry_id", entryID)
			return
		}
		s.executeIfNotDeletedOrPaused(jobID)
	}()
	return nil
}

// registerJob plans, commits and applies in one call, under whatever lock the
// caller holds. Its ONLY caller is Start's load loop, and that is a contract,
// not a coincidence: before s.cron.Start() robfig is not running, so Schedule
// appends to a slice instead of rendezvousing with the run loop (cron.go:168),
// and holding s.tbl.mu across it costs nothing. Every post-start writer must use
// the split form instead — planCronEntry under the lock, commitAndApplyCronEntry
// off it, with entryMu held around the pair (see entry_registration.go).
func (s *Scheduler) registerJob(j *Job) error {
	// Parse + derive first (pure), then hand the parsed schedule to robfig. The
	// old shape called AddFunc and then read the schedule back with
	// s.cron.Entry(entryID) purely to fill the cache — a blocking round trip with
	// the run loop, performed with s.tbl.mu held. The value was already in hand.
	//
	// The cache exists so per-tick applyJitterSched need not run sched.Next twice,
	// and so handleList's 1 Hz HasMissedSchedule fanout avoids re-parsing (#664,
	// #477). It is now always populated: a parse failure returns before anything
	// is registered, so the "entry vanished, cache it as zero" branch the
	// read-back needed has no remaining case.
	//
	// Every caller still holds s.tbl.mu across commitCronEntry; see cron_entry.go for
	// why that is latency rather than deadlock, and what removing it costs.
	p, err := planCronEntry(j.ID, j.Schedule, time.Now())
	if err != nil {
		return err
	}
	applyCronEntry(j, p, s.commitCronEntry(p))
	return nil
}

// newCronTickCallback returns the func() closure registered with robfig/cron
// for jobID (#785). No recover() here: the tick relies on robfig's Recover
// chain installed in NewScheduler; a refactor bypassing that chain MUST add
// one. Contracts fixed at this dispatch boundary:
//  1. captures jobID by value, never *Job — executeJobIDIfLive re-reads
//     s.tbl.jobs[jobID] under RLock so an UpdateJob remove+re-add resolves fresh;
//  2. delegates to executeJobIDIfLive, never executeOpt, so the deleted/paused
//     gate stays shared with TriggerNow;
//  3. viaTriggerNow=false / logSubject="cron" are pinned here; other fan-outs
//     must mint their own factory.
func (s *Scheduler) newCronTickCallback(jobID string) func() {
	return func() {
		s.executeJobIDIfLive(jobID, false /* viaTriggerNow */, "cron")
	}
}
