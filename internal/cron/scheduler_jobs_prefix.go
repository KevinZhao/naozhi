// scheduler_jobs_prefix.go: the IM-prefix-scoped per-job state changes
// (DeleteJob / PauseJob / ResumeJob). Same contracts as their by-ID twins in
// scheduler_jobs_byid.go; the job is the one in the caller's chat whose ID
// starts with idPrefix (jobTable.findByPrefixLocked).

package cron

// DeleteJob removes a job by ID prefix (scoped to the given chat).
func (s *Scheduler) DeleteJob(idPrefix, plat, chatID string) (*Job, error) {
	// Entry-lifecycle writer; see DeleteJobByID for why delete holds entryMu.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.finishMutation(s.tbl.mutateByPrefix(idPrefix, plat, chatID, mutDelete), mutDelete)
}

// PauseJob pauses a job by ID prefix. Same contract as PauseJobByID.
func (s *Scheduler) PauseJob(idPrefix, plat, chatID string) (*Job, error) {
	// Entry-lifecycle writer; see PauseJobByID for why pause holds entryMu.
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.finishMutation(s.tbl.mutateByPrefix(idPrefix, plat, chatID, mutPause), mutPause)
}

// ResumeJob resumes a paused job by ID prefix. Same contract as ResumeJobByID.
func (s *Scheduler) ResumeJob(idPrefix, plat, chatID string) (*Job, error) {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.finishMutation(s.tbl.mutateByPrefix(idPrefix, plat, chatID, mutResume), mutResume)
}
