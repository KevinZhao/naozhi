// scheduler_jobs_byid.go: exact-ID cron Job mutation path (move-only split
// of scheduler_jobs.go, #1282). Contains the shared lockedJobOp / jobSideEffect
// named-op types plus the withJobByID(Opt) framework and the three dashboard
// by-exact-ID mutators (DeleteJobByID / PauseJobByID / ResumeJobByID). The
// prefix-scoped twins (DeleteJob / PauseJob / ResumeJob) live in
// scheduler_jobs_prefix.go and reference lockedJobOp / jobSideEffect cross-file
// within package cron.
//
// No behaviour change. Methods stay on *Scheduler; lock order, saga rollback
// edges, and every critical section are byte-for-byte identical to the
// original scheduler_jobs.go.

package cron

import (
	"fmt"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

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
//
// R242-GO-3 (#548)：返回的 *Job 是 in-lock 时刻 *j 的 value-copy 的地址，
// 不再是 s.jobs[id] 的活指针。原本 j 被赋为 s.jobs[id] 后随 s.mu.Unlock
// 一起返回给调用方，调用方在锁外读取的 j.Field 可能与另一个 goroutine 的
// UpdateJob/SetJobPrompt 并发，触发 string header tear / data race。
// UpdateJob (line 655) 的 critical section 已经在锁内做 *j 复制，本 helper
// 把同样语义铺到 Delete/Pause/Resume 三入口；postCleanup 仍然收到锁外
// 拿到的 *jobSnapshot，副作用（router.Reset / runStore.DeleteJob）只读
// snapshot 的不可变字段（ID/Platform/ChatID）所以语义不变。
// withJobByIDOpts bundles the optional knobs withJobByID accepts so the
// signature stays a single function while individual callers (Pause /
// Resume / Delete / future ops) opt in to rollback semantics without
// touching unrelated paths. R20260527-COR-1 (#1272): historically op-
// success + persist-failure left in-memory state mutated and on-disk
// state stale, so a restart replayed the pre-op snapshot — divergence
// most visible on PauseJobByID (cron entry gone, j.Paused=true in
// memory, but disk shows Paused=false). When rollbackOnPersistErr is
// non-nil and persistJobsLocked returns an error, the helper invokes
// it under s.mu BEFORE releasing — restoring the in-memory mutation
// to match the un-persisted disk state — and skips postCleanup so the
// mutation's lock-released side effects (cron.Remove / router.Reset)
// don't fire on a rolled-back op.
//
// R20260527-GO-8 (#1300) op contract：op MUST be one of：
//
//  1. 全 mutate 成功 → return nil；in-memory 状态一致，persistJobsLocked
//     紧接其后落盘（marshal 失败时见 R20260527-COR-1 / #1272 的退路语义）。
//  2. 全无 mutate 失败 → return non-nil error；contract 是 op MUST NOT
//     leave any partial mutation on *j when returning error。否则 perr
//     透传给调用方但 in-memory 已脏 + persist 未触发，重启后状态发散。
//
// 现有 op 实现（pauseJobLocked / resumeJobLocked / deleteJobLocked-wrap）
// 均满足该不变量：失败检查放在所有写入之前。新增 op 时 reviewer 必须验
// 证：op 函数体内任意 return non-nil 路径之前没有 j.X = ... 写入；如果
// op 需要先尝试再回滚，应在 op 内部完成回滚后再 return。
// withJobByIDOpts knobs:
//
//   - op: in-lock mutation (must satisfy "all-or-nothing" — see contract
//     above). nil for pure-lookup callers.
//   - postCleanup: out-of-lock side effect (router.Reset, runStore.DeleteJob)
//     that runs UNCONDITIONALLY whenever op succeeded — even when
//     persistJobsLocked returned an error and rollbackOnPersistErr is nil.
//     Use this shape ONLY when the in-lock mutation is already past the
//     point of no return (DeleteJobByID's deleteJobLocked drops the *Job
//     from s.jobs irreversibly). For mutations where on-disk state is the
//     authoritative outcome (Pause/Resume), pair op with rollbackOnPersistErr
//     and leave postCleanup nil. R250-CR-16 (#1149).
//   - rollbackOnPersistErr: in-lock undo of the op's mutation when
//     persistJobsLocked fails. When non-nil and persist fails, this
//     restores *j BEFORE the snapshot copy and skips postCleanup so the
//     caller observes "no change applied".
//
// R249-ARCH-20 (#985): op and the two side-effect hooks share the bare
// `func(*Job)` / `func(*Job) error` shape, so a reviewer eyeballing a call
// site had nothing but argument position to tell an in-lock mutation apart
// from an out-of-lock cleanup. Naming them (lockedJobOp returns an error and
// runs UNDER s.mu; jobSideEffect returns nothing and runs lock-free) makes the
// two roles self-documenting and lets the compiler flag a swapped op-vs-cleanup
// argument (an error-returning closure can no longer be passed where a
// jobSideEffect is expected, and vice versa). Behaviour is unchanged — Go
// closures still satisfy these named types by assignability at every call
// site, so no call-site edits were needed.
type (
	// lockedJobOp is the in-lock mutation withJobByID(Opt) / withJobByPrefix
	// run while holding s.mu. It MUST be all-or-nothing: on a non-nil error
	// return it must leave *j unmutated (see the op contract godoc above).
	lockedJobOp func(j *Job) error
	// jobSideEffect is an out-of-lock hook (postCleanup / rollbackOnPersistErr).
	// It runs after s.mu is released (postCleanup) or as the in-lock undo of a
	// failed persist (rollbackOnPersistErr), and returns nothing.
	jobSideEffect func(j *Job)
)

type withJobByIDOpts struct {
	op                   lockedJobOp
	postCleanup          jobSideEffect
	rollbackOnPersistErr jobSideEffect
}

func (s *Scheduler) withJobByID(
	id string,
	op lockedJobOp,
	postCleanup jobSideEffect,
) (*Job, error) {
	return s.withJobByIDOpt(id, withJobByIDOpts{op: op, postCleanup: postCleanup})
}

// withJobByIDResult bundles the values withJobByIDOpt's locked critical
// section produces so the post-unlock control flow reads as named-field
// branches rather than five sibling `var` declarations mutated inside an
// IIFE. R249-CR-7 (#951): the prior shape declared save/snapshot/found/
// opErr/perr/rolledBack up front and assigned them from a closure, forcing
// the reader to scan both the IIFE body and the trailing branch ladder to
// reconstruct the state machine. Folding the locked work into
// lockedJobOp keeps the s.mu critical section in one named method and lets
// the caller branch on the returned struct.
type withJobByIDResult struct {
	save       func()
	snapshot   Job
	found      bool
	opErr      error
	perr       error
	rolledBack bool
}

// lockedJobOp runs the lookup + op + persist + (optional) rollback steps for
// withJobByIDOpt entirely under s.mu and returns the outcome. Splitting this
// out of the IIFE keeps every s.mu-guarded mutation in one named scope; the
// caller (withJobByIDOpt) is then pure post-unlock control flow.
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
	// R20260527-COR-1 (#1272): if the marshal step failed AFTER op
	// mutated *j, restore the in-memory mutation under s.mu so on-
	// disk state and in-memory state stay aligned. Run the rollback
	// before snapshotting so the returned snapshot reflects the
	// pre-op state — caller observes "no change applied" rather than
	// the half-applied mutation that motivated the divergence bug.
	if r.perr != nil && opts.rollbackOnPersistErr != nil {
		opts.rollbackOnPersistErr(j)
		r.rolledBack = true
	}
	// R242-GO-3 (#548): value-copy under s.mu so the caller (and
	// postCleanup) read a stable Job even if a concurrent
	// UpdateJob / SetJobPrompt mutates the live *j right after we
	// unlock. Mirrors UpdateJob's `return *j, save, perr` pattern.
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
	// R20260527-COR-1 (#1272): on rollback, skip postCleanup — its side
	// effects (cron.Remove for Pause, router.Reset for Delete) reflect a
	// mutation that is no longer in effect. Returning perr lets the caller
	// surface the persist failure as 5xx so the operator can retry.
	if rolledBack {
		return nil, perr
	}
	// R250-CR-16 (#1149): postCleanup runs UNCONDITIONALLY here — i.e.
	// even when persistJobsLocked returned perr != nil (without a paired
	// rollbackOnPersistErr hook). This is INTENTIONAL for DeleteJobByID:
	// deleteJobLocked already ran inside the locked section and dropped
	// the *Job from s.jobs, so the in-memory state is already past the
	// point of no return. The runStore.DeleteJob cleanup MUST run even
	// when the cron_jobs.json marshal failed; otherwise runs/<jobID>/
	// would leak entries for a job nobody can address again from the
	// dashboard (R238-GO-3). PauseJobByID / ResumeJobByID pass nil
	// postCleanup so this branch is a no-op for them — the asymmetry
	// is by design, not by accident. Future maintainers adding a new
	// withJobByID-shaped op MUST decide explicitly: cleanup after
	// success-only (use rollbackOnPersistErr to undo on failure) vs
	// cleanup-regardless (the DeleteJob shape, where the in-memory
	// mutation is already irreversibly applied).
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
	// R20260605B-CORR-6 (#1810): capture the cron entryID deleteJobLocked
	// snapshots under s.mu and remove it from cron in postCleanup (lock
	// released) so the unbuffered c.remove channel round-trip no longer
	// happens under the s.mu write hold — matching the pause/resume hoist.
	var removeEntryID cronEntryID
	return s.withJobByID(
		id,
		// op：调 deleteJobLocked 移除 in-memory 记录；不返回错误（删除路径无校验）。
		func(j *Job) error {
			removeEntryID = s.deleteJobLocked(j)
			return nil
		},
		// postCleanup：锁外做 cron.Remove + router.Reset + runStore.DeleteJob +
		// runningJobs reclaim。R244-ARCH-13 (#1053): 共享 helper，与
		// plat+chat-based DeleteJob 走同一条 side-effect 顺序，详见
		// deleteJobPostCleanup godoc（R240-GO-1 / R238-GO-3 / R242-ARCH-15）。
		func(j *Job) { s.deleteJobPostCleanup(j.ID, removeEntryID) },
	)
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use).
//
// R236-QA-03 (#537): cron.Remove is hoisted to postCleanup so s.mu is
// released before the unbuffered c.remove channel send completes —
// matches the lock-order discipline ListAllJobsWithNextRun's godoc
// pins. The closure captures the cleanup func returned by
// pauseJobLocked under s.mu so the entryID we're removing is the
// exact one snapshotted at the in-memory mutation point (no
// re-read race after Unlock).
//
// R20260527-COR-1 (#1272): if persistJobsLocked fails AFTER pauseJobLocked
// flipped j.Paused / cleared j.entryID, the disk-vs-memory divergence
// surfaces on restart as the unpaused job replaying from disk. Capture
// the pre-op (entryID, Paused) tuple so a rollback callback can restore
// in-memory state to match the un-persisted disk view; the helper also
// skips postCleanup (the cron.Remove hoist) on rollback so the cron entry
// stays alive and the next tick still fires the now-active job.
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	var pauseCleanup func()
	var prevEntryID cronEntryID
	var prevPaused bool
	var captured bool
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact
		// pre-op view; pauseLocked mutates j.entryID + j.Paused only
		// after this read so a concurrent reader can never observe a
		// torn write.
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
		// Only restore if op actually ran and captured the pre-op view —
		// guards against a future refactor that might invoke rollback on
		// the "op never ran" path.
		if !captured {
			return
		}
		j.entryID = prevEntryID
		j.Paused = prevPaused
		// pauseCleanup is the cron.Remove hoist returned by pauseJobLocked.
		// Drop it so postCleanup's safety net (which is now skipped on
		// rollback in withJobByIDOpt) cannot accidentally fire even if a
		// future refactor reorders the flow.
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
// R20260526-GO-001 (#1226): registerJob mutates j.entryID + j.cachedPeriod
// before resumeJobLocked flips j.Paused, so a persistJobsLocked failure
// after op-success would leave in-memory state with a live cron entry +
// Paused=false while disk still shows Paused=true — restart would then
// re-register the schedule on top of the surviving runtime entry,
// producing a double-fire. Capture the pre-op state under s.mu and
// install a rollback that removes the cron entry and restores
// (entryID, cachedPeriod, Paused) so the in-memory view matches the
// un-persisted disk view. Mirrors PauseJobByID's rollback contract.
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	var prevEntryID cronEntryID
	var prevCachedPeriod time.Duration
	var prevCachedSched robfigcron.Schedule
	var prevPaused bool
	var captured bool
	// CR-1 (R250531-CR-1): entryID to remove AFTER withJobByIDOpt returns
	// (i.e. after s.mu is released). rollback runs under s.mu; calling
	// s.cron.Remove there causes a lock-order inversion — robfig/cron.Remove
	// sends on the unbuffered c.remove channel which can only be drained by
	// the cron-tick goroutine, and that goroutine calls executeJobIDIfLive →
	// s.mu.RLock. Pattern mirrors PauseJobByID's pauseCleanup hoist (#537).
	var removeEntryID cronEntryID
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact pre-op
		// view; resumeJobLocked → registerJob mutates entryID +
		// cachedPeriod + cachedSched only after this read so a concurrent
		// reader can never observe a torn write.
		prevEntryID = j.entryID
		prevCachedPeriod = j.cachedPeriod
		prevCachedSched = j.cachedSched // CR-3 (R250531-CR-3): snapshot cachedSched
		prevPaused = j.Paused
		captured = true
		return s.resumeJobLocked(j)
	}
	rollback := func(j *Job) {
		// Only restore if op actually ran and captured the pre-op view —
		// guards against a future refactor that might invoke rollback on
		// the "op never ran" path.
		if !captured {
			return
		}
		// CR-1: capture the freshly-registered entryID for removal OUTSIDE
		// s.mu. Do NOT call s.cron.Remove here — we are under s.mu and
		// cron.Remove sends on an unbuffered channel drained only by the
		// cron-tick goroutine that itself acquires s.mu.RLock → deadlock.
		removeEntryID = j.entryID
		j.entryID = prevEntryID
		j.cachedPeriod = prevCachedPeriod
		j.cachedSched = prevCachedSched // CR-3: restore cachedSched
		j.Paused = prevPaused
	}
	snap, err := s.withJobByIDOpt(id, withJobByIDOpts{
		op:                   op,
		rollbackOnPersistErr: rollback,
	})
	// CR-1: remove the orphaned cron entry now that s.mu is released.
	// removeEntryID is non-zero only when rollback fired (persist failed
	// after op succeeded and registered a new entry). The zero check is
	// defensive; robfig/cron.Remove(0) is a no-op, but being explicit
	// makes the intent clear.
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	return snap, err
}
