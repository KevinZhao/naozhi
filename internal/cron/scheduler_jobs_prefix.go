// scheduler_jobs_prefix.go: IM-prefix-scoped cron Job mutation path — the
// withJobByPrefix framework, the IM-scoped mutators (DeleteJob / PauseJob /
// ResumeJob) and findByPrefixLocked. lockedJobOp / jobSideEffect are defined
// in scheduler_jobs_byid.go.

package cron

import (
	"fmt"
	"strings"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// withJobByPrefixOpts bundles the optional knobs withJobByPrefix accepts,
// mirroring withJobByIDOpts. rollbackOnPersistErr: in-lock undo of the op's
// mutation when persistJobsLocked fails; restores *j BEFORE the snapshot copy
// and skips postCleanup so the caller observes "no change applied". nil for
// callers (DeleteJob) that do not need it.
type withJobByPrefixOpts struct {
	rollbackOnPersistErr jobSideEffect
}

// withJobByPrefixResult bundles the locked-section outputs of withJobByPrefix
// so the post-unlock flow branches on named fields — the prefix twin of
// withJobByIDResult.
type withJobByPrefixResult struct {
	save       func()
	snapshot   Job
	findErr    error
	opErr      error
	perr       error
	rolledBack bool
}

// lockedJobPrefixOp runs find-by-prefix + op + persist + optional rollback for
// withJobByPrefix entirely under s.mu, mirroring lockedJobOp on the by-ID path.
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
	// Persist failed after op mutated *j: undo under s.mu so disk and memory
	// stay aligned (mirrors withJobByIDOpt).
	if r.perr != nil && rollback != nil {
		rollback(j)
		r.rolledBack = true
	}
	// Value-copy under s.mu so postCleanup and the caller read a stable Job
	// even if a concurrent UpdateJob / SetJobPrompt mutates *j after unlock.
	r.snapshot = *j
	return r
}

// withJobByPrefix is the IM-prefix counterpart to withJobByID, shared by
// DeleteJob / PauseJob / ResumeJob: lock → findByPrefixLocked → op → persist
// → unlock → postCleanup → save. postCleanup must NOT run under s.mu (router
// callbacks may re-take it); save() runs after postCleanup so a persist
// failure still leaves the side effects committed (runStore.DeleteJob fires
// even when persist fails). Error precedence: find miss → op error → persist
// error (postCleanup already ran unless rollbackOnPersistErr reversed the op,
// in which case it is skipped — #1272) → success (*Job, nil).
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
	// On rollback skip postCleanup — the cron Remove hoist must not fire when
	// the in-memory mutation was reversed. perr surfaces as 5xx for retry.
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
	// deleteJobLocked snapshots the cron entryID under s.mu; postCleanup runs
	// the cron Remove after unlock (#1810).
	var removeEntryID cronEntryID
	return s.withJobByPrefix(
		idPrefix, plat, chatID,
		func(j *Job) error {
			removeEntryID = s.deleteJobLocked(j)
			return nil
		},
		// Shared with DeleteJobByID so both delete paths run the same
		// side-effect sequence (#1053).
		func(j *Job) { s.deleteJobPostCleanup(j.ID, removeEntryID) },
		withJobByPrefixOpts{},
	)
}

// PauseJob pauses a job by ID prefix. Same contract as PauseJobByID: the cron
// Remove from pauseJobLocked runs in postCleanup after s.mu is released
// (#537), and a persist failure rolls back (entryID, Paused) and skips
// postCleanup so the cron entry stays alive and the still-active job keeps
// firing instead of becoming a ghost-paused job on restart (#1272).
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
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
		// Drop pauseCleanup so nothing fires the cron Remove we chose NOT to
		// run (the entry must stay alive since the pause was not persisted).
		pauseCleanup = nil
	}
	return s.withJobByPrefix(idPrefix, plat, chatID, op, postCleanup, withJobByPrefixOpts{
		rollbackOnPersistErr: rollback,
	})
}

// ResumeJob resumes a paused job by ID prefix. Same contract as ResumeJobByID
// (#1226): resumeJobLocked → registerJob mutates entryID/cachedPeriod/
// cachedSched and flips Paused before persist, so a persist failure rolls the
// pre-op state back and the orphaned entry is removed AFTER withJobByPrefix
// returns — a cron Remove under s.mu would deadlock against the cron-tick
// goroutine that drains c.remove and takes s.mu.RLock.
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	var prevEntryID cronEntryID
	var prevCachedPeriod time.Duration
	var prevCachedSched robfigcron.Schedule
	var prevPaused bool
	var captured bool
	// Non-zero only when rollback fired; the cron Remove must happen after
	// s.mu is released (see godoc).
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
		// cron Remove here would deadlock.
		removeEntryID = j.entryID
		j.entryID = prevEntryID
		j.cachedPeriod = prevCachedPeriod
		j.cachedSched = prevCachedSched
		j.Paused = prevPaused
	}
	snap, err := s.withJobByPrefix(idPrefix, plat, chatID, op, nil, withJobByPrefixOpts{
		rollbackOnPersistErr: rollback,
	})
	// Remove the orphaned cron entry now that s.mu is released. Non-zero only
	// when rollback fired; Remove(0) would be a no-op anyway.
	if removeEntryID != 0 {
		s.cron.Remove(removeEntryID)
	}
	return snap, err
}

// findByPrefixLocked finds a job by ID prefix scoped to a specific chat.
// Returns exactly one of: (job, nil) — unique match in (plat, chatID);
// (nil, ErrJobNotFound) — no match, OR a full-length ID exists in a different
// chat (masked so callers cannot probe foreign jobs by ID); (nil,
// ErrAmbiguousPrefix) — a short prefix matches ≥2 jobs; the message lists the
// colliding IDs so the operator can disambiguate (#950).
//
// LOCK: caller MUST hold s.mu (read or write). A full-length hex ID takes the
// O(1) s.jobs fast path (#705); a partial prefix scans only
// s.jobsByChat[chat], bounded by maxJobsPerChat rather than all jobs (#558).
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
		// Full-length ID with no map hit still falls through to the scan: a
		// corrupt store or future ID-width bump could leave a 16-char prefix
		// that is not a full ID, so the scan tail is the safety net.
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
