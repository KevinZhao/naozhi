// scheduler_jobs.go: cron Job CRUD path — public mutation APIs, list /
// lookup APIs, and the robfig-cron entry registration (registerJob).
// Run-time hot path lives in scheduler_run.go, lifecycle in scheduler.go.

package cron

import (
	"fmt"
	"log/slog"
	"slices"
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
	// while it is half-made. Taken before s.mu per the lock order.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()

	// addJobAcquiringLock owns s.mu (acquire + deferred unlock) so every
	// early-return path releases the lock in one place. It plans the cron entry
	// but does not commit it: on a persist failure there is therefore no robfig
	// entry to roll back — deleteJobLocked under the still-held lock undoes
	// everything, and the rollbackEntryID hand-off this path used to need is
	// gone with the reason for it.
	save, stub, plan, perr := s.addJobAcquiringLock(j)
	if perr != nil {
		return perr
	}
	// Commit outside s.mu (the robfig rendezvous), then write the id back. In
	// the window between insert and apply the job is visible with entryID 0 —
	// the same transient UpdateJob's schedule swap already shows — and entryMu
	// keeps every other entry writer out of it.
	if plan != nil {
		s.commitAndApplyCronEntry(*plan)
	}
	save()
	// Use the fields snapshotted under s.mu rather than re-reading *j after
	// unlock: a concurrent UpdateJob on the same id could race the reads (#1068).
	s.registerStubByValue(stub.id, stub.workDir, stub.prompt, stub.lastSessionID)
	return nil
}

// addJobStubFields is the lock-held snapshot of the fields AddJob passes to
// registerStubByValue, so a concurrent UpdateJob / SetJobPrompt cannot
// mutate them after addJobAcquiringLock releases s.mu (#1068).
type addJobStubFields struct {
	id            string
	workDir       string
	prompt        string
	lastSessionID string
}

// addJobAcquiringLock performs the AddJob mutation. Unlike the *Locked
// siblings (caller-holds-lock convention) it owns s.mu itself: acquires at
// entry and defers Unlock so every early return releases in one place.
//
// plan is non-nil for an active job and is committed by AddJob AFTER this
// function releases s.mu. Because the commit has not happened yet, a persist
// failure rolls everything back with deleteJobLocked alone — there is no
// robfig entry to remove, which is what retired the rollbackEntryID hand-off
// (#1810) this signature used to carry.
func (s *Scheduler) addJobAcquiringLock(j *Job) (save func(), stub addJobStubFields, plan *cronEntryPlan, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.jobs) >= s.maxJobs {
		return nil, addJobStubFields{}, nil, fmt.Errorf("max cron jobs reached (%d)", s.maxJobs)
	}

	// Per-chat limit so one chat cannot exhaust the global quota. O(1) via
	// s.chatJobCount, kept in lock-step with s.jobs by addToChatIndexLocked /
	// deleteJobLocked (#661).
	chatKey := chatKeyFor(j.Platform, j.ChatID)
	if s.chatJobCount[chatKey] >= s.maxJobsPerChat {
		return nil, addJobStubFields{}, nil, fmt.Errorf("per-chat cron limit reached (%d)", s.maxJobsPerChat)
	}

	id, err := generateID()
	if err != nil {
		// crypto/rand 失败透传：AddJob 是公共入口，应表现为请求拒绝而非 panic；
		// rand 整体失效时重试只会复现同一错误，提早 bail (#706)。
		return nil, addJobStubFields{}, nil, fmt.Errorf("cron: generate job id: %w", err)
	}
	j.ID = id
	// Retry on ID collision, bounded so a degenerate generateID cannot spin
	// under s.mu. Warn once on the first collision; the same ID twice in a row
	// is proof of a deterministic generator → bail at Error (#493).
	prevID := j.ID
	for i := 0; i < 10; i++ {
		if _, exists := s.jobs[j.ID]; !exists {
			break
		}
		if i == 0 {
			slog.Warn("cron: job ID collision, retrying", "attempt", i+1, "job_id", j.ID)
		}
		retryID, retryErr := generateID()
		if retryErr != nil {
			// 同上：rand 中途失效，提早返回比继续循环更诚实。
			return nil, addJobStubFields{}, nil, fmt.Errorf("cron: regenerate job id (retry %d): %w", i+1, retryErr)
		}
		if retryID == prevID {
			// Same ID twice in a row: the source is not random and the remaining
			// retries would fail identically.
			slog.Error("cron: deterministic ID generator detected; bailing early",
				"attempt", i+1, "id", retryID)
			return nil, addJobStubFields{}, nil, fmt.Errorf("cron: deterministic ID generator (id %q repeated)", retryID)
		}
		prevID = retryID
		j.ID = retryID
	}
	if _, exists := s.jobs[j.ID]; exists {
		return nil, addJobStubFields{}, nil, fmt.Errorf("cron: failed to generate unique job ID after 10 attempts")
	}
	j.CreatedAt = time.Now()

	if !j.Paused {
		// Plan only — parse the schedule and derive the cache — so a bad spec is
		// rejected before anything mutates. The commit happens in AddJob after
		// this function releases s.mu.
		p, perr := planCronEntry(j.ID, j.Schedule, time.Now())
		if perr != nil {
			return nil, addJobStubFields{}, nil, perr
		}
		plan = &p
	}
	s.jobs[j.ID] = j
	// Per-chat counter + index move in lockstep with s.jobs; deleteJobLocked
	// is the paired inverse (also used by the rollback below).
	s.addToChatIndexLocked(j)
	save, perr := s.persistJobsLocked()
	if perr != nil {
		// Persist failed after the map insertion. The cron entry was only
		// planned, never committed, so deleteJobLocked under the still-held
		// s.mu undoes everything — there is no robfig state to clean up. The
		// router stub is only registered after a successful save, so no router
		// cleanup is needed either.
		s.deleteJobLocked(j)
		return nil, addJobStubFields{}, nil, perr
	}
	// Snapshot under s.mu what registerStubByValue reads so AddJob need not
	// re-read *j after unlock (#1068).
	stub = addJobStubFields{
		id:            j.ID,
		workDir:       j.WorkDir,
		prompt:        j.Prompt,
		lastSessionID: j.LastSessionID,
	}
	return save, stub, plan, nil
}

// addToChatIndexLocked records a job into the per-chat side indexes that must
// move in lockstep with s.jobs (chatJobCount cap counter, jobsByChat lookup
// slice, sortedJobIDs). Caller MUST hold s.mu.Lock() and have already
// inserted j into s.jobs. deleteJobLocked is the paired inverse.
func (s *Scheduler) addToChatIndexLocked(j *Job) {
	key := chatKeyFor(j.Platform, j.ChatID)
	s.chatJobCount[key]++
	s.jobsByChat[key] = append(s.jobsByChat[key], j)
	s.insertSortedJobID(j.ID)
}

// insertSortedJobID keeps s.sortedJobIDs ascending via binary-search insert
// so marshalJobsLocked can iterate without re-sorting on every persist.
// Idempotent on a duplicate ID so a malformed disk load keeps the slice 1:1
// with the map. Caller must hold s.mu.Lock() (#1598).
func (s *Scheduler) insertSortedJobID(id string) {
	i, found := slices.BinarySearch(s.sortedJobIDs, id)
	if found {
		return
	}
	s.sortedJobIDs = slices.Insert(s.sortedJobIDs, i, id)
}

// removeSortedJobID drops id from s.sortedJobIDs preserving order. No-op if
// absent so a double-delete (rollback path) cannot panic. Caller must hold
// s.mu.Lock().
func (s *Scheduler) removeSortedJobID(id string) {
	if i, found := slices.BinarySearch(s.sortedJobIDs, id); found {
		s.sortedJobIDs = slices.Delete(s.sortedJobIDs, i, i+1)
	}
}

// deleteJobLocked performs the in-memory side effects of removing a job:
// snapshot+zero the cron entry and drop the map/index entries. It returns the
// captured cron entryID (0 if none) so the caller runs the cron Remove AFTER
// releasing s.mu — Remove sends on the unbuffered c.remove channel that only
// run() drains, so doing it under s.mu would hold the write lock across a
// cron-select round-trip (#1810). Caller must hold s.mu.Lock().
//
// Intentionally does NOT delete from s.runningJobs (a concurrent execute may
// still hold the CAS gate; see cleanupRunningJobIfIdle) and MUST NOT call
// router.Reset (its callbacks may re-take s.mu) — callers do that after unlock.
func (s *Scheduler) deleteJobLocked(j *Job) (removeEntryID cronEntryID) {
	removeEntryID = j.entryID
	j.entryID = 0
	if _, present := s.jobs[j.ID]; present {
		delete(s.jobs, j.ID)
		// Paired removal from the sorted-ID slice, guarded by the same
		// membership check so a double-delete cannot disturb it.
		s.removeSortedJobID(j.ID)
		// Paired decrement for the per-chat counter; the membership guard keeps
		// a double-delete from driving it negative (which would silently disable
		// the per-chat cap). Drop the key at zero so the map tracks live chats.
		key := chatKeyFor(j.Platform, j.ChatID)
		if n := s.chatJobCount[key]; n > 1 {
			s.chatJobCount[key] = n - 1
		} else {
			delete(s.chatJobCount, key)
		}
		// Paired remove from the per-chat index. Swap-and-shrink is fine:
		// findByPrefixLocked reports ambiguity instead of picking a winner, so
		// order is irrelevant. Drop the key when the slice empties.
		if list := s.jobsByChat[key]; len(list) > 0 {
			for i, p := range list {
				if p == j {
					last := len(list) - 1
					list[i] = list[last]
					list[last] = nil // help GC drop the pointer
					list = list[:last]
					break
				}
			}
			if len(list) == 0 {
				delete(s.jobsByChat, key)
			} else {
				s.jobsByChat[key] = list
			}
		}
	}
	return removeEntryID
}

// deleteJobPostCleanup runs the lock-free side effects that must follow
// deleteJobLocked, shared by DeleteJobByID and DeleteJob. Caller MUST NOT
// hold s.mu:
//   - cron Remove of removeEntryID (0 = no entry; Remove(0) is a no-op) keeps
//     the unbuffered c.remove send off the s.mu write hold (#1810);
//   - resetRouterStub: router.Reset callbacks may re-enter s.mu;
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
	s.cleanupRunningJobIfIdle(jobID)
}

// pauseJobLocked transitions a job to Paused under s.mu. Returns
// ErrJobAlreadyPaused without mutation if already paused (callers map it to
// 409). The cron Remove is NOT done here: robfig's Remove sends on the
// unbuffered c.remove channel, so it is returned as cronCleanup for callers
// to run AFTER releasing s.mu (#537). cronCleanup is never nil and is
// idempotent (the captured entryID is consumed on the first call), so
// callers can defer it unconditionally.
func (s *Scheduler) pauseJobLocked(j *Job) (cronCleanup func(), err error) {
	if j.Paused {
		return func() {}, fmt.Errorf("%w: id %q", ErrJobAlreadyPaused, j.ID)
	}
	// Snapshot the entryID for post-unlock removal and zero it under lock so
	// concurrent ListAllJobsWithNextRun / NextRun / TriggerNow snapshots see
	// the entry-removed state before cron's own table catches up.
	captured := j.entryID
	j.entryID = 0
	j.Paused = true
	if captured == 0 {
		return func() {}, nil
	}
	return func() { s.cron.Remove(captured) }, nil
}

// resumePlanLocked transitions a paused job back to active under s.mu and
// returns the cron-entry plan the caller must commit AFTER releasing the lock
// (commitAndApplyCronEntry). Returns ErrJobNotPaused, or a parse error, both
// without mutation.
//
// The registration itself used to happen right here, which meant three things
// at once: a robfig rendezvous under s.mu, a persist-failure rollback that had
// to un-register a live entry from outside the lock (#1226's double-fire), and
// entryID/cached* snapshots threaded through every caller to make that rollback
// exact. Deferring the commit until after persist dissolves all three — a
// persist failure now happens BEFORE any entry exists, so the rollback is one
// field write, and the window it opens (Paused=false, entryID=0) is fenced by
// entryMu, which every caller holds.
func (s *Scheduler) resumePlanLocked(j *Job) (cronEntryPlan, error) {
	if !j.Paused {
		return cronEntryPlan{}, fmt.Errorf("%w: id %q", ErrJobNotPaused, j.ID)
	}
	p, err := planCronEntry(j.ID, j.Schedule, time.Now())
	if err != nil {
		return cronEntryPlan{}, err
	}
	j.Paused = false
	return p, nil
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

// applyTo writes every non-nil JobUpdate field onto j. Caller must hold s.mu
// (j is the *Job from s.jobs). Schedule is intentionally NOT applied here:
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
		// window where the IIFE below releases s.mu on purpose. entryMu is what
		// keeps two such swaps from each registering an entry and leaving the job
		// firing on the union of two schedules — see entry_registration.go.
		//
		// Taken only when a schedule change is REQUESTED, so prompt/workdir edits
		// never touch it, and taken before s.mu to honour the lock order. Held to
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

	// Critical section is an IIFE with deferred unlock. robfig/cron's Remove and
	// Schedule rendezvous with the run loop over unbuffered channels, and Remove
	// is a full round trip. That is a LATENCY problem, not a deadlock one: run()
	// never takes runningMu and startJob hands each tick to a fresh goroutine, so
	// nothing reaches s.mu holding runningMu and s.mu → runningMu cannot close a
	// cycle (the same reasoning as the schedNeedsRereg block below, which is the
	// one place that spells it out). Holding s.mu across the rendezvous would make
	// every registry reader — the dashboard's 1 Hz list, each tick's own jobs[id]
	// lookup — queue behind the run loop. So the IIFE only applies fields,
	// snapshots the old entryID (zeroing j.entryID), and persists; cron ops run
	// post-unlock.
	var (
		schedRemoveEntryID cronEntryID
		schedOldSchedule   string
		schedNewSchedule   string
		schedNeedsRereg    bool
	)
	result, save, err := func() (Job, func(), error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		j, ok := s.jobs[id]
		if !ok {
			return Job{}, nil, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		}

		// Snapshot the live Job by value BEFORE mutating so a persist failure
		// can restore it exactly (including the Notify pointer applyTo replaces
		// and the runtime-only entryID/cachedSched, which are correct to keep
		// because this path aborts before any re-registration).
		preUpdate := *j
		upd.applyTo(j)

		// agentcore §4.4 guardrail on the EFFECTIVE post-patch combination:
		// placement=sandbox with a work_dir must fail atomically — restore the
		// pre-patch job before persist and before any re-registration.
		if placementIsSandbox(j.Placement) && j.WorkDir != "" {
			*j = preUpdate
			return Job{}, nil, ErrSandboxWorkDir
		}

		if upd.Schedule != nil && *upd.Schedule != j.Schedule {
			// Snapshot the old schedule for rollback and the entryID to remove
			// post-unlock. entryID is runtime-only, so persisting 0 is safe.
			schedOldSchedule = j.Schedule
			schedNewSchedule = *upd.Schedule
			j.Schedule = schedNewSchedule
			if !j.Paused {
				// Clear under lock so concurrent readers see entryID=0 now
				// (NextRun is zero until registerJob runs post-unlock).
				schedRemoveEntryID = j.entryID
				j.entryID = 0
				j.cachedPeriod = 0
				j.cachedSched = nil
				schedNeedsRereg = true
			}
		}

		save, perr := s.persistJobsLocked()
		if perr != nil {
			// Restore the pre-update snapshot under the same lock so no reader
			// observes the half-applied edit; the caller returns at err != nil
			// so the post-unlock re-registration never runs.
			*j = preUpdate
			return Job{}, nil, perr
		}
		// Value-copy while still under lock so the caller sees a stable result
		// even if another goroutine mutates the job right after we unlock.
		return *j, save, perr
	}()
	if err != nil {
		return nil, err
	}
	// Both robfig calls — the Remove of the old entry and the Schedule of the
	// new one — run with s.mu released: they rendezvous with the run loop, and
	// holding the registry lock across that parks every reader behind it
	// (latency, not deadlock; run() never takes runningMu). entryMu, held since
	// the top of UpdateJob for schedule changes, is what keeps another entry
	// writer out of the entryID=0 window in between.
	if schedNeedsRereg {
		if schedRemoveEntryID != 0 {
			s.cron.Remove(schedRemoveEntryID)
		}
		// Plan is pure (no lock, no robfig) and cannot fail here in practice:
		// validateSchedule at the top of UpdateJob uses the same parser. The
		// branch below is defence for the day those two drift apart, and it has
		// to exist because by now the old entry is gone — failing silently would
		// leave a job that never fires again.
		if p, planErr := planCronEntry(id, schedNewSchedule, time.Now()); planErr == nil {
			s.commitAndApplyCronEntry(p)
		} else {
			// Roll the in-memory schedule back, best-effort restore the old
			// entry, and persist the rolled-back state so disk agrees. If even
			// the OLD schedule no longer plans, the job cannot fire — mark it
			// Paused so the dashboard shows the degraded state instead of an
			// active job with no entry.
			oldPlan, oldErr := planCronEntry(id, schedOldSchedule, time.Now())
			s.mu.Lock()
			if j := s.jobs[id]; j != nil {
				j.Schedule = schedOldSchedule
				if oldErr != nil {
					slog.Error("cron: failed to restore previous schedule after UpdateJob rollback",
						"job_id", id, "schedule", schedOldSchedule, "err", oldErr)
					j.Paused = true
				}
				if save2, perr2 := s.persistJobsLocked(); perr2 == nil {
					s.mu.Unlock()
					save2()
				} else {
					s.mu.Unlock()
					slog.Error("cron: re-persist after UpdateJob rollback failed",
						"job_id", id, "err", perr2)
				}
			} else {
				s.mu.Unlock()
			}
			if oldErr == nil {
				s.commitAndApplyCronEntry(oldPlan)
			}
			return nil, fmt.Errorf("re-register cron: %w", planErr)
		}
	}
	// Refresh LastSessionID from the live job: result was snapshotted before
	// registerJob ran and a concurrent recordTerminalResult may have written
	// a newer session id, which would anchor the sidebar stub on a stale one.
	if schedNeedsRereg {
		if live, ok := s.lastSessionID(id); ok {
			result.LastSessionID = live
		}
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
	// swaps its cron entry. Taken unconditionally (before s.mu, per the lock
	// order) rather than peeking at Paused first — the peek would be its own
	// race, and this path is a dashboard edit with nothing to contend for.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()

	// The critical section is an IIFE with deferred unlock so a panic inside
	// resumePlanLocked cannot leave s.mu held. The plan commit and save() run
	// post-unlock so the robfig rendezvous stays outside s.mu.
	var resumePlan *cronEntryPlan
	type stubFields struct {
		workDir     string
		lastSession string
	}
	save, stub, err := func() (func(), stubFields, error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		j, ok := s.jobs[id]
		if !ok {
			return nil, stubFields{}, fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		}
		if j.Prompt != "" {
			// Auto-fill only: never overwrite. Return a sentinel so the no-op is
			// observable instead of a silent 200 (#1503).
			return nil, stubFields{}, ErrPromptAlreadySet
		}

		j.Prompt = prompt
		// Capture identity fields under lock so the post-unlock stub refresh
		// reads stable values even if a concurrent UpdateJob mutates *Job.
		sf := stubFields{workDir: j.WorkDir, lastSession: j.LastSessionID}
		waspaused := j.Paused
		if j.Paused {
			// Shared helper keeps the plan + Paused transition consistent with
			// Resume/UpdateJob. The plan is committed after this IIFE returns,
			// off the lock.
			p, rerr := s.resumePlanLocked(j)
			if rerr != nil {
				j.Prompt = "" // rollback: Prompt was empty before this call
				return nil, stubFields{}, rerr
			}
			resumePlan = &p
		}
		saveFn, perr := s.persistJobsLocked()
		if perr != nil {
			// Roll back in-memory state before releasing the lock so the live
			// view never reflects an un-persisted mutation. The resume plan was
			// never committed, so no cron entry exists yet: restoring Paused is
			// the whole rollback, where this path once had to hoist a cron
			// Remove out of the lock (#537).
			j.Prompt = ""
			if waspaused && !j.Paused {
				j.Paused = true
				resumePlan = nil
			}
			return nil, stubFields{}, perr
		}
		return saveFn, sf, nil
	}()

	if err != nil {
		return err
	}
	// Commit the resume outside s.mu; entryMu (held since entry) fences the
	// entryID=0 window against every other entry writer.
	if resumePlan != nil {
		s.commitAndApplyCronEntry(*resumePlan)
	}
	save()
	// Refresh the router stub so the sidebar reflects the new prompt now
	// rather than at the next executeJob tick.
	s.registerStubByValue(id, stub.workDir, prompt, stub.lastSession)
	slog.Info("cron job prompt set", "job_id", id, "prompt_len", len(prompt))
	return nil
}

// NextRun returns the next scheduled run time for a job. entryID is resolved
// under s.mu.RLock and s.mu is released BEFORE s.cron.Entry(): Entry walks
// Entries(), which round-trips the dispatcher's snapshot channel, so holding
// s.mu across it would invert the lock order the cron dispatch path takes
// (cron-internal → execute → recordResult → s.mu.Lock) — the same discipline
// ListAllJobsWithNextRun follows (#1117).
//
// When j.entryID is zero (a *Job that did not flow through AddJob / loadJobs,
// e.g. a deserialised snapshot) fall back to the live s.jobs[j.ID] record so
// the dashboard does not render a misleading "01/01 00:00" (#784).
func (s *Scheduler) NextRun(j *Job) time.Time {
	if j == nil {
		return time.Time{}
	}
	// Resolve entryID under RLock, release, then read cron (see godoc).
	// TriggerNow deliberately keeps its cross-lock: it needs one consistent
	// instant for the entry-gone check against a racing DeleteJob.
	s.mu.RLock()
	entryID := j.entryID
	if entryID == 0 && j.ID != "" {
		if live, ok := s.jobs[j.ID]; ok {
			entryID = live.entryID
		}
	}
	s.mu.RUnlock()
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
// Caller must hold s.mu (read or write) so the read cannot race a concurrent
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
	s.mu.RLock()
	// Gate triggerWG.Add behind the stopped flag: stopWithCtx sets s.stopped
	// before draining triggerWG, and an in-flight HandleTrigger could otherwise
	// Add(1) from zero concurrently with Wait, violating the WaitGroup contract
	// and letting a trigger goroutine escape the drain barrier (#2012).
	if s.stopped.Load() {
		s.mu.RUnlock()
		return ErrSchedulerStopped
	}
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
	// Add to triggerWG before releasing s.mu so a concurrent Stop() cannot see
	// an empty WaitGroup and return before our goroutine starts; paired with
	// the single deferred Done() in the goroutine body.
	s.triggerWG.Add(1)

	// Hold s.mu.RLock across cron.Entry + the entry-gone check so a racing
	// DeleteJob is observed at one consistent instant (cron's lock never calls
	// back into scheduler code). entryID==0 means paused/unregistered, never
	// "gone". TriggerNow 跳过 cron chain 直接 executeOpt（"run now" 不要 jitter）；
	// 重叠由 jobRunningGuard CAS 覆盖，panic 由 recordTriggerNowPanic recover 覆盖。
	entryGone := entryID != 0 && s.cronEntryGoneLocked(entryID)
	s.mu.RUnlock()

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
// and holding s.mu across it costs nothing. Every post-start writer must use
// the split form instead — planCronEntry under the lock, commitAndApplyCronEntry
// off it, with entryMu held around the pair (see entry_registration.go).
func (s *Scheduler) registerJob(j *Job) error {
	// Parse + derive first (pure), then hand the parsed schedule to robfig. The
	// old shape called AddFunc and then read the schedule back with
	// s.cron.Entry(entryID) purely to fill the cache — a blocking round trip with
	// the run loop, performed with s.mu held. The value was already in hand.
	//
	// The cache exists so per-tick applyJitterSched need not run sched.Next twice,
	// and so handleList's 1 Hz HasMissedSchedule fanout avoids re-parsing (#664,
	// #477). It is now always populated: a parse failure returns before anything
	// is registered, so the "entry vanished, cache it as zero" branch the
	// read-back needed has no remaining case.
	//
	// Every caller still holds s.mu across commitCronEntry; see cron_entry.go for
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
//     s.jobs[jobID] under RLock so an UpdateJob remove+re-add resolves fresh;
//  2. delegates to executeJobIDIfLive, never executeOpt, so the deleted/paused
//     gate stays shared with TriggerNow;
//  3. viaTriggerNow=false / logSubject="cron" are pinned here; other fan-outs
//     must mint their own factory.
func (s *Scheduler) newCronTickCallback(jobID string) func() {
	return func() {
		s.executeJobIDIfLive(jobID, false /* viaTriggerNow */, "cron")
	}
}
