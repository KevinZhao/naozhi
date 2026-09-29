// scheduler_jobs_byid.go: the per-job state changes by exact ID
// (DeleteJobByID / PauseJobByID / ResumeJobByID, for the dashboard) and
// finishMutation, the lock-free half every by-ID and by-prefix mutation shares.
// The in-lock half is jobTable.mutateByID / mutateByPrefix; prefix-scoped
// twins live in scheduler_jobs_prefix.go.

package cron

// finishMutation runs the side effects of a mutation the table already made,
// after its lock is released, and writes the snapshot. Error precedence:
// lookup → the transition does not apply → persist. A pause or resume whose
// persist failed was undone in the table and hands back no entry to remove or
// commit, which would reflect a change no longer in effect (#1272). A delete's cleanup runs even then: the job is gone from memory, and
// runs/<jobID>/ would otherwise leak for a job nobody can address (#1149).
// Delete and pause retire the robfig entry here, off the registry lock
// (#537, #1810); resume commits its entry here, after persist succeeded.
func (s *Scheduler) finishMutation(r mutationResult, kind mutationKind) (*Job, error) {
	if r.lookupErr != nil {
		return nil, r.lookupErr
	}
	if r.opErr != nil {
		return nil, r.opErr
	}
	switch kind {
	case mutDelete:
		s.deleteJobPostCleanup(r.job.ID, r.removeEntry)
	case mutPause:
		if r.removeEntry != 0 {
			s.cron.Remove(r.removeEntry)
		}
	case mutResume:
		if r.plan != nil {
			s.commitAndApplyCronEntry(*r.plan)
		}
	}
	if r.persistErr != nil {
		return nil, r.persistErr
	}
	s.save(r.snap)
	job := r.job
	return &job, nil
}

// DeleteJobByID removes a job by exact ID (unscoped, for dashboard use).
func (s *Scheduler) DeleteJobByID(id string) (*Job, error) {
	// Entry-lifecycle writer: entryMu keeps a delete out of another writer's
	// plan → commit window, which is what lets commitAndApplyCronEntry write
	// the id back without re-checking that the job still exists.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.finishMutation(s.tbl.mutateByID(id, mutDelete), mutDelete)
}

// PauseJobByID pauses a job by exact ID (unscoped, for dashboard use). If the
// persist fails, the job keeps its entry and stays active; otherwise a restart
// would replay the unpaused job from disk (#1272).
func (s *Scheduler) PauseJobByID(id string) (*Job, error) {
	// Entry-lifecycle writer (it removes the entry): hold entryMu so a pause
	// cannot land inside another writer's plan → commit window — where its
	// Remove would see entryID=0, no-op, and leave the freshly committed entry
	// alive on a job that reads as paused.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.finishMutation(s.tbl.mutateByID(id, mutPause), mutPause)
}

// ResumeJobByID resumes a paused job by exact ID (unscoped, for dashboard use).
//
// The cron entry is committed after persist succeeded and the registry lock
// is released, so a persist failure happens before any entry exists and the
// rollback is one field write; the Paused=false/entryID=0 window in between is
// fenced by entryMu against every other entry writer.
func (s *Scheduler) ResumeJobByID(id string) (*Job, error) {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.finishMutation(s.tbl.mutateByID(id, mutResume), mutResume)
}
