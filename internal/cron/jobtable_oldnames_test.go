package cron

// The registry's write side moved to jobTable (#2959); these forward the old
// Scheduler names so the tests that drive them compile unchanged. Test builds
// only — production code has none of these names. S2 moves the tests onto the
// jobTable methods and deletes this file.

func (s *Scheduler) addToChatIndexLocked(j *Job) { s.tbl.indexLocked(j) }

func (s *Scheduler) deleteJobLocked(j *Job) cronEntryID { return s.tbl.deleteLocked(j) }

func (s *Scheduler) findByPrefixLocked(idPrefix, plat, chatID string) (*Job, error) {
	return s.tbl.findByPrefixLocked(idPrefix, plat, chatID)
}

func (s *Scheduler) marshalJobsLocked() ([]byte, error) { return s.tbl.marshalLocked() }

func (s *Scheduler) snapshotJobsForSaveLocked() jobsSnapshot { return s.tbl.snapshotForSaveLocked() }

// pauseJobLocked keeps the old shape: the robfig Remove as a cleanup to run
// after the lock is released.
func (s *Scheduler) pauseJobLocked(j *Job) (func(), error) {
	e, err := s.tbl.pauseLocked(j)
	if err != nil || e == 0 {
		return func() {}, err
	}
	return func() { s.cron.Remove(e) }, nil
}
