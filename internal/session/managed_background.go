package session

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// reattachInfo is what a shim reattach knew of the process it attached:
// when the shim started the CLI, and the task frames its replay held. A
// replay reaches no event log (its frames have no times), so a background
// task that ended while naozhi was down ended only there.
type reattachInfo struct {
	proc     processIface
	cliStart time.Time
	tasks    []clievent.EventEntry
}

// setReattached records proc's reattach info; the session must hold proc.
func (s *ManagedSession) setReattached(proc processIface, cliStart time.Time, tasks []clievent.EventEntry) {
	s.reattached.Store(&reattachInfo{proc: proc, cliStart: cliStart, tasks: tasks})
}

// reattachedFor is the reattach info of proc, nil when proc was spawned or
// the info is another process's.
func (s *ManagedSession) reattachedFor(proc processIface) *reattachInfo {
	if r := s.reattached.Load(); r != nil && proc != nil && r.proc == proc {
		return r
	}
	return nil
}

// ProcessStartedAt is when the session's CLI process started: when naozhi
// spawned it, or, for one reattached through its shim after a restart,
// when the shim did. Zero without a process. Its entries from before then
// are an earlier process's: a background task they started died with it.
func (s *ManagedSession) ProcessStartedAt() time.Time {
	proc := s.loadProcess()
	if r := s.reattachedFor(proc); r != nil && !r.cliStart.IsZero() {
		return r.cliStart
	}
	return s.ProcessAttachedAt()
}

// ReplayedTasks are the task frames the reattach of the session's process
// replayed, as entries without times; nil for a spawned process.
func (s *ManagedSession) ReplayedTasks() []clievent.EventEntry {
	if r := s.reattachedFor(s.loadProcess()); r != nil {
		return r.tasks
	}
	return nil
}

// ProcessLastEventAt is the session's process's latest live event, zero
// without a process or before its first.
func (s *ManagedSession) ProcessLastEventAt() time.Time {
	if p := s.loadProcess(); p != nil {
		return p.LastEventAt()
	}
	return time.Time{}
}
