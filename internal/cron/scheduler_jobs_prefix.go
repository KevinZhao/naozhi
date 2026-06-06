// scheduler_jobs_prefix.go: IM-prefix-scoped cron Job mutation path (move-only
// split of scheduler_jobs.go, #1282). Contains the withJobByPrefix framework,
// the three IM-scoped mutators (DeleteJob / PauseJob / ResumeJob), and the
// findByPrefixLocked lookup helper. The shared lockedJobOp / jobSideEffect
// named-op types live in scheduler_jobs_byid.go and are referenced cross-file
// within package cron.
//
// No behaviour change. Methods stay on *Scheduler; lock order, saga rollback
// edges, and every critical section are byte-for-byte identical to the
// original scheduler_jobs.go.

package cron

import (
	"fmt"
	"strings"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

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
// rollbackOnPersistErr (optional, pass nil for DeleteJob callers that do not
// need it): if non-nil and persistJobsLocked returns an error, it is called
// under s.mu BEFORE the snapshot copy — restoring *j to its pre-op state so
// disk (un-persisted) and memory stay aligned. postCleanup is skipped on
// rollback (mirrors withJobByIDOpt's contract — R20260527-COR-1 / #1272).
//
// Error precedence (preserved from the originals):
//   - find miss      → (nil, find err)
//   - op error       → (nil, op err)        ; persist + postCleanup skipped
//   - persist error  → (nil, persist err)   ; postCleanup ALREADY ran
//     (unless rollbackOnPersistErr is set — then postCleanup is skipped)
//   - success        → (*Job, nil)
//
// withJobByPrefixOpts bundles the optional knobs withJobByPrefix accepts,
// mirroring withJobByIDOpts so the IM-prefix path stays symmetric with the
// exact-ID path. A struct (instead of a variadic) gives compile-time
// protection: callers cannot silently pass more than one rollback hook.
//
//   - rollbackOnPersistErr: in-lock undo of the op's mutation when
//     persistJobsLocked fails. When non-nil and persist fails, this restores
//     *j BEFORE the snapshot copy and skips postCleanup so the caller observes
//     "no change applied". nil for callers (DeleteJob) that do not need it.
type withJobByPrefixOpts struct {
	rollbackOnPersistErr jobSideEffect // R249-ARCH-20 (#985)
}

// withJobByPrefixResult bundles the locked-section outputs of
// withJobByPrefix so the post-unlock flow reads as named-field branches
// rather than five sibling vars mutated inside an IIFE — the prefix-path
// twin of withJobByIDResult. R249-CR-7 (#951).
type withJobByPrefixResult struct {
	save       func()
	snapshot   Job
	findErr    error
	opErr      error
	perr       error
	rolledBack bool
}

// lockedJobPrefixOp runs the find-by-prefix + op + persist + (optional)
// rollback steps for withJobByPrefix entirely under s.mu, mirroring
// lockedJobOp on the by-ID path. Splitting it out of the IIFE keeps every
// s.mu-guarded mutation in one named scope.
func (s *Scheduler) lockedJobPrefixOp(idPrefix, plat, chatID string, op func(j *Job) error, rollback func(j *Job)) withJobByPrefixResult {
	var r withJobByPrefixResult
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.findByPrefixLocked(idPrefix, plat, chatID)
	if err != nil {
		r.findErr = err
		return r
	}
	if op != nil {
		if err := op(j); err != nil {
			r.opErr = err
			return r
		}
	}
	r.save, r.perr = s.persistJobsLocked()
	// R20260531070014-CR-1/CR-2: mirror withJobByIDOpt's rollback contract —
	// if persistJobsLocked failed after op mutated *j, restore in-memory state
	// under s.mu so disk (un-persisted) and memory stay aligned.
	if r.perr != nil && rollback != nil {
		rollback(j)
		r.rolledBack = true
	}
	// R242-GO-3 mirror (#548): value-copy under s.mu so postCleanup and
	// the caller read a stable Job even if a concurrent UpdateJob /
	// SetJobPrompt mutates the live *j right after Unlock. Matches
	// withJobByIDOpt's "snapshot = *j" pattern. [R250531-CR-2]
	r.snapshot = *j
	return r
}

func (s *Scheduler) withJobByPrefix(
	idPrefix, plat, chatID string,
	op lockedJobOp,
	postCleanup jobSideEffect,
	opts withJobByPrefixOpts,
) (*Job, error) {
	r := s.lockedJobPrefixOp(idPrefix, plat, chatID, op, opts.rollbackOnPersistErr)
	save, snapshot, perr, rolledBack := r.save, r.snapshot, r.perr, r.rolledBack

	if r.findErr != nil {
		return nil, r.findErr
	}
	if r.opErr != nil {
		return nil, r.opErr
	}
	// R20260531070014-CR-1/CR-2: on rollback skip postCleanup (mirrors
	// withJobByIDOpt — the cron.Remove hoist must not fire when the in-memory
	// mutation was reversed). Return perr so the caller surfaces the persist
	// failure as a 5xx and the operator can retry.
	if rolledBack {
		return nil, perr
	}
	if postCleanup != nil {
		postCleanup(&snapshot)
	}
	if perr != nil {
		return nil, perr
	}
	save()
	return &snapshot, nil
}

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	// R20260605B-CORR-6 (#1810): mirror DeleteJobByID — deleteJobLocked
	// snapshots the cron entryID under s.mu and postCleanup runs s.cron.Remove
	// after the lock is released.
	var removeEntryID cronEntryID
	return s.withJobByPrefix(
		idPrefix, plat, chatID,
		func(j *Job) error {
			removeEntryID = s.deleteJobLocked(j)
			return nil
		},
		// R244-ARCH-13 (#1053): the IM-prefix DeleteJob path is the cron
		// alias side of the same lifecycle as DeleteJobByID, so both share
		// the deleteJobPostCleanup helper rather than open-coding the
		// cron.Remove + router.Reset + runStore.DeleteJob + runningJobs-reclaim
		// sequence twice (R240-GO-1 / R238-GO-3 / R242-ARCH-15 all documented there).
		func(j *Job) { s.deleteJobPostCleanup(j.ID, removeEntryID) },
		withJobByPrefixOpts{},
	)
}

// PauseJob pauses a job by ID prefix.
//
// R236-QA-03 (#537): same lock-order pattern as PauseJobByID — the
// cron.Remove returned by pauseJobLocked runs in postCleanup so the
// unbuffered c.remove channel send doesn't happen under s.mu.
//
// R20260531070014-CR-1: mirror PauseJobByID's rollbackOnPersistErr contract
// (#1272). If persistJobsLocked fails after pauseJobLocked already mutated
// (j.entryID=0, j.Paused=true), restore the pre-op (entryID, Paused) under
// s.mu so disk (un-persisted Paused=false) and memory stay aligned — preventing
// the "ghost-paused job that never fires" split-brain on restart.
// postCleanup (cron.Remove hoist) is skipped on rollback so the cron entry
// stays alive and the next tick still fires the now-active job.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	var pauseCleanup func()
	var prevEntryID cronEntryID
	var prevPaused bool
	var captured bool
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact
		// pre-op view; pauseLocked mutates j.entryID + j.Paused only
		// after this read.
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
		// Drop pauseCleanup so no future code path accidentally fires
		// the cron.Remove that we are choosing NOT to run (the entry
		// must stay alive since the pause was not persisted).
		pauseCleanup = nil
	}
	return s.withJobByPrefix(idPrefix, plat, chatID, op, postCleanup, withJobByPrefixOpts{
		rollbackOnPersistErr: rollback,
	})
}

// ResumeJob resumes a paused job by ID prefix.
//
// R20260531070014-CR-2: mirror ResumeJobByID's rollbackOnPersistErr contract
// (#1226). resumeJobLocked → registerJob mutates j.entryID + j.cachedPeriod +
// j.cachedSched and then flips j.Paused=false BEFORE persistJobsLocked runs.
// A persist failure after that op-success path would leave in-memory state with
// a live cron entry + Paused=false while disk still shows Paused=true — on
// restart the scheduler re-registers the schedule on top of the surviving
// runtime entry, producing a double-fire. Capture the pre-op state under s.mu
// and install a rollback that removes the cron entry and restores
// (entryID, cachedPeriod, cachedSched, Paused) so in-memory matches disk.
//
// Lock-order: rollback runs under s.mu; calling s.cron.Remove there would
// send on the unbuffered c.remove channel drained only by the cron-tick
// goroutine, which itself calls s.mu.RLock → deadlock. Mirror ResumeJobByID's
// removeEntryID pattern: capture the freshly-registered entryID inside the
// rollback closure and call s.cron.Remove AFTER withJobByPrefix returns
// (s.mu released).
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	var prevEntryID cronEntryID
	var prevCachedPeriod time.Duration
	var prevCachedSched robfigcron.Schedule
	var prevPaused bool
	var captured bool
	// removeEntryID is non-zero only when rollback fired; cron.Remove must
	// be called after withJobByPrefix returns (s.mu released) to avoid
	// the lock-order inversion described above.
	var removeEntryID cronEntryID
	op := func(j *Job) error {
		// Snapshot under s.mu so the rollback restores the exact pre-op
		// view; resumeJobLocked → registerJob mutates entryID +
		// cachedPeriod + cachedSched only after this read.
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
		// Capture the freshly-registered entryID for removal OUTSIDE s.mu.
		// Do NOT call s.cron.Remove here — we are under s.mu and cron.Remove
		// sends on an unbuffered channel drained only by the cron-tick goroutine
		// that itself acquires s.mu.RLock → deadlock.
		removeEntryID = j.entryID
		j.entryID = prevEntryID
		j.cachedPeriod = prevCachedPeriod
		j.cachedSched = prevCachedSched
		j.Paused = prevPaused
	}
	snap, err := s.withJobByPrefix(idPrefix, plat, chatID, op, nil, withJobByPrefixOpts{
		rollbackOnPersistErr: rollback,
	})
	// Remove the orphaned cron entry now that s.mu is released. removeEntryID
	// is non-zero only when rollback fired (persist failed after op succeeded
	// and registered a new entry). robfig/cron.Remove(0) is a no-op, but being
	// explicit about the guard makes the intent clear.
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	return snap, err
}

// findByPrefixLocked finds a job by ID prefix scoped to a specific chat.
//
// RETURNS (R249-CR-6, #950): exactly one of —
//   - (job, nil)                      the prefix uniquely identifies one job
//     in the (plat, chatID) scope.
//   - (nil, ErrJobNotFound)           no job in the scope matches the prefix,
//     OR a full-length ID exists but in a different chat scope (the foreign
//     job is masked as NotFound so callers can't probe its existence by ID).
//   - (nil, ErrAmbiguousPrefix)       a short prefix (typically 1-2 chars from
//     the IM-typed `naozhi cron pause <prefix>` flow) matches ≥2 jobs in the
//     scope; the wrapped message lists the colliding IDs so the operator can
//     disambiguate. Callers should errors.Is-check this and surface a
//     "please disambiguate" hint rather than treating it as NotFound.
//
// COMPLEXITY: the partial-prefix scan is linear in the number of jobs in the
// target chat (s.jobsByChat[chat]), bounded by maxJobsPerChat — NOT the full
// s.jobs table. The full-ID fast path below is O(1).
//
// LOCK: caller MUST hold s.mu (read or write). The body iterates the
// per-chat slice from s.jobsByChat directly without taking the mutex;
// every in-tree caller (DeleteJob / PauseJob / ResumeJob) already holds
// s.mu.Lock() across the find + mutate + persist window, so the *Locked
// suffix is a documentation contract, not a behaviour change. Renamed
// under R20260526-GO-002 to match the package convention (deleteJobLocked
// / pauseJobLocked / persistJobsLocked / …) so future callers see the
// locking requirement without grepping the call graph.
//
// R242-GO-9 (#558): scan is bounded by s.jobsByChat[chat] (typically
// 1-5 jobs/chat) rather than the full s.jobs map (up to maxJobsHardCap=
// 500). This drops the lock-time prefix scan to O(jobs-in-this-chat) so
// withJobByPrefix doesn't pin s.mu across the entire job table on every
// IM-prefix delete/pause/resume.
//
// R246-GO-16 (#705): full-ID fast path. When idPrefix is a complete
// hex job ID (length 2*hexIDEntropyBytes = 16) we hit s.jobs directly —
// O(1) map lookup instead of an O(N) range scan. Dashboard / HTTP
// callers already round-trip the full ID (the truncated prefix form
// only appears in the IM-typed CLI flow `naozhi cron pause abc` where
// the operator types a partial ID), so the common case is ID-shaped
// and benefits. The scan path is preserved verbatim for the partial-
// prefix case so the ambiguous-match error still fires identically.
// The Platform / ChatID match still has to gate the result — a full
// ID may hit the wrong chat scope (cross-chat probe) and must return
// ErrJobNotFound rather than the foreign job. Note we still hold the
// caller-supplied write lock during the lookup; the dashboard 1Hz
// read path is in s.mu.RLock (ListJobs / ListAllJobsWithNextRun) and
// the win is shorter blocking — the partial-prefix scan stays an
// honest O(N) tail.
func (s *Scheduler) findByPrefixLocked(idPrefix, plat, chatID string) (*Job, error) {
	if len(idPrefix) == 2*hexIDEntropyBytes {
		if j, ok := s.jobs[idPrefix]; ok {
			if j.Platform == plat && j.ChatID == chatID {
				return j, nil
			}
			// Full ID exists but in a different chat scope — surface
			// the same NotFound error the scan path would, so cross-
			// chat callers can't probe foreign-job existence by ID.
			return nil, fmt.Errorf("%w: prefix %q", ErrJobNotFound, idPrefix)
		}
		// Full-length ID with no map hit: still fall through to the
		// scan path. A pathological store load could in theory keep a
		// 16-char prefix that is NOT a full ID (e.g. data corruption
		// or a future ID-width bump where the operator types a 16-
		// char prefix of a 32-char ID), so the scan tail acts as the
		// safety net rather than short-circuiting on the map miss.
	}
	var matches []*Job
	for _, j := range s.jobsByChat[chatKeyFor(plat, chatID)] {
		if strings.HasPrefix(j.ID, idPrefix) {
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
