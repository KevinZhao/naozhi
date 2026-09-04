// scheduler_jobs_byid.go: exact-ID cron Job mutation path. Holds the shared
// lockedJobOp / jobSideEffect named-op types, the withJobByID(Opt) framework,
// and the dashboard by-exact-ID mutators (DeleteJobByID / PauseJobByID /
// ResumeJobByID). Prefix-scoped twins live in scheduler_jobs_prefix.go.

package cron

import (
	"fmt"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// lockedJobOp / jobSideEffect name the two closure roles withJobByID(Opt) and
// withJobByPrefix accept, so a swapped op-vs-cleanup argument fails to compile
// instead of silently running a mutation lock-free (#985).
type (
	// lockedJobOp is the in-lock mutation withJobByID(Opt) / withJobByPrefix
	// run while holding s.mu. It MUST be all-or-nothing: on a non-nil error
	// return it must leave *j unmutated (see withJobByIDOpts).
	lockedJobOp func(j *Job) error
	// jobSideEffect is an out-of-lock hook (postCleanup / rollbackOnPersistErr).
	// It runs after s.mu is released (postCleanup) or as the in-lock undo of a
	// failed persist (rollbackOnPersistErr), and returns nothing.
	jobSideEffect func(j *Job)
)

// withJobByIDOpts bundles the knobs withJobByIDOpt accepts:
//   - op: in-lock mutation; MUST be all-or-nothing — on a non-nil error it
//     must leave *j unmutated, else memory is dirty while persist never ran
//     and a restart diverges (#1300). nil for pure-lookup callers.
//   - postCleanup: lock-free side effect that runs whenever op succeeded, EVEN
//     if persist failed and no rollback hook is set (#1149). Use only when the
//     in-lock mutation is past the point of no return (DeleteJobByID).
//   - rollbackOnPersistErr: in-lock undo when persistJobsLocked fails; restores
//     *j before the snapshot copy and skips postCleanup so the caller observes
//     "no change applied" (#1272). Pair with op for Pause/Resume.
type withJobByIDOpts struct {
	op                   lockedJobOp
	postCleanup          jobSideEffect
	rollbackOnPersistErr jobSideEffect
}

// withJobByID 是 DeleteJobByID / PauseJobByID / ResumeJobByID 的共用执行框架：
//  1. 持 s.mu.Lock 查 id，缺失返回 ErrJobNotFound 包装错误；
//  2. 锁内调 op(j)，成功后 persistJobsLocked 拿 save 闭包；
//  3. 释放 s.mu，调 postCleanup（router.Reset 等锁外副作用），再 save() 落盘。
//
// 返回的 *Job 是锁内 value-copy 的地址而非 s.jobs[id] 活指针，避免调用方在
// 锁外读到并发 UpdateJob/SetJobPrompt 的 tear (#548)。返回：找不到 →
// (nil, ErrJobNotFound)；op 失败 → (nil, err)；persist 失败 → (nil, perr)。
func (s *Scheduler) withJobByID(
	id string,
	op lockedJobOp,
	postCleanup jobSideEffect,
) (*Job, error) {
	return s.withJobByIDOpt(id, withJobByIDOpts{op: op, postCleanup: postCleanup})
}

// withJobByIDResult bundles the outputs of lockedJobOp's critical section so
// withJobByIDOpt's post-unlock flow branches on named fields (#951).
type withJobByIDResult struct {
	save       func()
	snapshot   Job
	found      bool
	opErr      error
	perr       error
	rolledBack bool
}

// lockedJobOp runs lookup + op + persist + optional rollback for withJobByIDOpt
// entirely under s.mu; withJobByIDOpt is then pure post-unlock control flow.
func (s *Scheduler) lockedJobOp(id string, opts withJobByIDOpts) withJobByIDResult {
	var r withJobByIDResult
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		r.perr = fmt.Errorf("%w: id %q", ErrJobNotFound, id)
		return r
	}
	if opts.op != nil {
		if err := opts.op(j); err != nil {
			r.opErr = err
			return r
		}
	}
	r.found = true
	r.save, r.perr = s.persistJobsLocked()
	// Persist failed after op mutated *j: undo under s.mu before snapshotting
	// so disk and memory stay aligned and the caller observes "no change
	// applied" (#1272).
	if r.perr != nil && opts.rollbackOnPersistErr != nil {
		opts.rollbackOnPersistErr(j)
		r.rolledBack = true
	}
	// Value-copy under s.mu so caller and postCleanup read a stable Job even
	// if a concurrent UpdateJob / SetJobPrompt mutates *j after unlock (#548).
	r.snapshot = *j
	return r
}

func (s *Scheduler) withJobByIDOpt(id string, opts withJobByIDOpts) (*Job, error) {
	r := s.lockedJobOp(id, opts)
	save, snapshot, found, perr, rolledBack := r.save, r.snapshot, r.found, r.perr, r.rolledBack

	if r.opErr != nil {
		return nil, r.opErr
	}
	if !found {
		return nil, perr
	}
	// On rollback skip postCleanup: its side effects (cron Remove for Pause,
	// router.Reset for Delete) reflect a mutation no longer in effect.
	if rolledBack {
		return nil, perr
	}
	// postCleanup runs UNCONDITIONALLY — even when persist failed without a
	// rollback hook. Intentional for DeleteJobByID: deleteJobLocked already
	// dropped the *Job from s.jobs, so runStore.DeleteJob MUST still run or
	// runs/<jobID>/ leaks for a job nobody can address again (#1149).
	if opts.postCleanup != nil {
		opts.postCleanup(&snapshot)
	}
	if perr != nil {
		return nil, perr
	}
	save()
	return &snapshot, nil
}

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	// deleteJobLocked snapshots the cron entryID under s.mu; the cron Remove
	// runs in postCleanup after unlock so the unbuffered c.remove send stays
	// off the write hold (#1810).
	var removeEntryID cronEntryID
	return s.withJobByID(
		id,
		// op：deleteJobLocked 移除 in-memory 记录；删除路径无校验，不返回错误。
		func(j *Job) error {
			removeEntryID = s.deleteJobLocked(j)
			return nil
		},
		// postCleanup：锁外 cron.Remove + router.Reset + runStore.DeleteJob +
		// runningJobs reclaim，与 DeleteJob 共享 deleteJobPostCleanup (#1053)。
		func(j *Job) { s.deleteJobPostCleanup(j.ID, removeEntryID) },
	)
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use).
//
// The cron Remove returned by pauseJobLocked runs in postCleanup, after s.mu
// is released (#537). If persist fails after pauseJobLocked mutated
// (entryID=0, Paused=true), rollback restores the pre-op tuple and
// postCleanup is skipped so the cron entry stays alive and the still-active
// job keeps firing; otherwise a restart would replay the unpaused job from
// disk (#1272).
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	var pauseCleanup func()
	var prevEntryID cronEntryID
	var prevPaused bool
	var captured bool
	op := func(j *Job) error {
		// Snapshot under s.mu before pauseJobLocked mutates entryID/Paused so
		// rollback restores the exact pre-op view.
		prevEntryID = j.entryID
		prevPaused = j.Paused
		captured = true
		c, err := s.pauseJobLocked(j)
		pauseCleanup = c
		return err
	}
	postCleanup := func(_ *Job) {
		if pauseCleanup != nil {
			pauseCleanup()
		}
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view.
		if !captured {
			return
		}
		j.entryID = prevEntryID
		j.Paused = prevPaused
		// Drop the cron Remove hoist so nothing can fire it for a pause that
		// was never persisted.
		pauseCleanup = nil
	}
	return s.withJobByIDOpt(id, withJobByIDOpts{
		op:                   op,
		postCleanup:          postCleanup,
		rollbackOnPersistErr: rollback,
	})
}

// ResumeJobByID resumes a paused job by exact ID (unscoped, for dashboard use).
//
// registerJob mutates entryID/cachedPeriod/cachedSched before resumeJobLocked
// flips Paused, so a persist failure would leave a live cron entry +
// Paused=false in memory while disk says Paused=true — a restart would then
// re-register on top of the surviving entry and double-fire. The rollback
// restores the pre-op state and the orphaned entry is removed after unlock
// (#1226).
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	var prevEntryID cronEntryID
	var prevCachedPeriod time.Duration
	var prevCachedSched robfigcron.Schedule
	var prevPaused bool
	var captured bool
	// entryID to remove AFTER withJobByIDOpt returns: rollback runs under s.mu
	// and robfig's Remove sends on the unbuffered c.remove channel drained only
	// by the cron-tick goroutine, which takes s.mu.RLock → deadlock (#537).
	var removeEntryID cronEntryID
	op := func(j *Job) error {
		// Snapshot under s.mu before resumeJobLocked → registerJob mutates
		// entryID/cachedPeriod/cachedSched so rollback restores the exact view.
		prevEntryID = j.entryID
		prevCachedPeriod = j.cachedPeriod
		prevCachedSched = j.cachedSched
		prevPaused = j.Paused
		captured = true
		return s.resumeJobLocked(j)
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view.
		if !captured {
			return
		}
		// Capture the freshly-registered entryID for removal OUTSIDE s.mu; a
		// cron Remove here would deadlock (see removeEntryID above).
		removeEntryID = j.entryID
		j.entryID = prevEntryID
		j.cachedPeriod = prevCachedPeriod
		j.cachedSched = prevCachedSched
		j.Paused = prevPaused
	}
	snap, err := s.withJobByIDOpt(id, withJobByIDOpts{
		op:                   op,
		rollbackOnPersistErr: rollback,
	})
	// Remove the orphaned cron entry now that s.mu is released. Non-zero only
	// when rollback fired; Remove(0) would be a no-op anyway.
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	return snap, err
}
