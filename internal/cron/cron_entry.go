package cron

// cron_entry.go — the boundary between the job registry (guarded by s.mu) and
// robfig/cron's run loop.
//
// Two robfig calls rendezvous with that run loop:
//
//   - Schedule (and AddFunc, which wraps it) sends the new entry on an
//     unbuffered channel — cron.go:171, and only while the scheduler is running.
//   - Entry(id) is the worse one: Entries() sends a reply channel and then BLOCKS
//     waiting for the answer (cron.go:180-183). A full round trip.
//
// Neither can deadlock. run() never takes runningMu, and startJob hands the
// callback to a fresh goroutine, so nothing reaches s.mu while holding runningMu
// and s.mu → runningMu cannot close a cycle. The cost is latency, and it lands
// on every reader of the job registry — the dashboard's 1 Hz list, every tick's
// own jobs[id] lookup — because they queue behind s.mu while it waits on a
// goroutine that may be busy dispatching other jobs.
//
// The round trip is gone (the parsed schedule is handed in, nothing to read
// back), and so is the one-way send under s.mu: every post-start writer now
// plans under the lock, commits off it, and applies the id back under a short
// re-acquire — with entryMu held around the whole span so no other entry
// writer can slip into the entryID=0 window (entry_registration.go). Deferring
// the commit until after persist also dissolved the rollback machinery those
// transactions used to need: a persist failure now happens before any entry
// exists, so there is nothing to un-register. registerJob survives only for
// Start's load loop, where robfig is not yet running and Schedule is an
// append, not a rendezvous.

import (
	"fmt"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// cronEntryPlan is a registration derived without touching robfig or any lock.
type cronEntryPlan struct {
	jobID string
	sched robfigcron.Schedule
	// period is the cached interval applyJitterSched reads so it need not run
	// sched.Next twice on every tick.
	period time.Duration
}

// planCronEntry parses spec and derives the cached period. Pure — no robfig, no
// locks — so a caller holding s.mu may call it.
//
// cronParser is the same grammar robfig would have used: the Cron is built
// without WithParser, so its parser is robfig's standardParser, which is
// NewParser of exactly cronParseOptions. Parsing here rather than inside AddFunc
// is what makes the Entry(id) read-back unnecessary.
func planCronEntry(jobID, spec string, now time.Time) (cronEntryPlan, error) {
	sched, err := cronParser.Parse(spec)
	if err != nil {
		// Same wrapper text AddFunc's error carried, so callers that surface it
		// (UpdateJob's rollback logs it) read unchanged.
		return cronEntryPlan{}, fmt.Errorf("register cron: %w", err)
	}
	return cronEntryPlan{jobID: jobID, sched: sched, period: schedulePeriodFromSched(sched, now)}, nil
}

// cronCommitHook runs immediately before the one robfig call that rendezvous
// with the run loop. Test-only seam: the lock-discipline test installs a hook
// that proves s.mu is not held by the committing goroutine — the rule this file
// exists to make structural, checked by a machine instead of a comment.
var cronCommitHook func()

// commitCronEntry hands the plan to robfig and returns the entry id. This is the
// one call that rendezvous with the run loop.
//
// MUST NOT run under s.mu. Every production caller holds entryMu instead (see
// entry_registration.go), which is what makes the pattern below safe.
func (s *Scheduler) commitCronEntry(p cronEntryPlan) cronEntryID {
	if cronCommitHook != nil {
		cronCommitHook()
	}
	// FuncJob so the chain installed in NewScheduler (Recover +
	// SkipIfStillRunning) wraps this exactly as AddFunc did: Schedule applies
	// c.chain.Then(cmd) for both entry points.
	return s.cron.Schedule(p.sched, robfigcron.FuncJob(s.newCronTickCallback(p.jobID)))
}

// applyCronEntry writes a committed registration onto the job. Pure assignment;
// the caller holds s.mu.
func applyCronEntry(j *Job, p cronEntryPlan, id cronEntryID) {
	j.entryID = id
	j.cachedSched = p.sched
	j.cachedPeriod = p.period
}

// commitAndApplyCronEntry is the out-of-lock half of a registration: commit the
// plan to robfig, then take s.mu just long enough to write the id back onto the
// live job.
//
// The caller holds entryMu across the whole plan → commit → apply span, and
// that is what makes the blind write-back safe: every writer that could delete
// the job, change its schedule, or pause it also holds entryMu, so between the
// plan and this apply the job's entry-relevant state is frozen. The lookup can
// still miss in exactly one case — Stop tore the table down — and then the
// freshly committed entry must be removed or it ticks for a job nobody can see.
func (s *Scheduler) commitAndApplyCronEntry(p cronEntryPlan) {
	id := s.commitCronEntry(p)
	s.mu.Lock()
	j, ok := s.jobs[p.jobID]
	if ok {
		applyCronEntry(j, p, id)
	}
	s.mu.Unlock()
	if !ok {
		s.cron.Remove(id)
	}
}
