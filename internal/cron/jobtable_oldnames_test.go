package cron

import "time"

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

// registerJob plans, commits and applies in one call, under whatever lock the
// caller holds — safe only before s.cron.Start(), when robfig's Schedule
// appends to a slice instead of rendezvousing with the run loop. Tests use it
// to seed an entry on a job they built by hand.
func (s *Scheduler) registerJob(j *Job) error {
	p, err := planCronEntry(j.ID, j.Schedule, time.Now())
	if err != nil {
		return err
	}
	applyCronEntry(j, p, s.commitCronEntry(p))
	return nil
}

// persistJobsLocked is the closure form of jobTable.persistLocked: marshal
// under the caller's s.tbl.mu, save after it is released.
func (s *Scheduler) persistJobsLocked() (func(), error) {
	m, err := s.tbl.persistLocked()
	if err != nil {
		return nil, err
	}
	return func() { s.save(m) }, nil
}
