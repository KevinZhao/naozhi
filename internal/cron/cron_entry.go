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
// The round trip is now gone: the parsed schedule the cache wanted is the value
// we already hand in, so there is nothing to read back. What remains is the
// one-way send, still performed under s.mu by all six callers of registerJob.
// Removing that is the other half of the work and is NOT a call-site rename: each
// of the six sits inside a transaction with its own persist-and-roll-back
// ordering (AddJob's rollbackEntryID, SetJobPrompt's pauseRollbackCleanup,
// UpdateJob's re-register-the-old-schedule path). The plan/commit/apply split
// below is the shape those transactions will use, one site at a time.

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

// commitCronEntry hands the plan to robfig and returns the entry id. This is the
// one call that rendezvous with the run loop.
func (s *Scheduler) commitCronEntry(p cronEntryPlan) cronEntryID {
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
