package cron

// adoption.go — cron run adoption, Phase 1 (#2712 PR B,
// docs/rfc/cron-run-adoption.md v2 §4).
//
// A `naozhi upgrade` used to swallow whatever cron run was in flight: Phase 0
// (#2691) records it as interrupted, but the CLI usually outlives the restart
// — the shim exists precisely so it can — and PR A (#2748) made the answer
// survivable: the reconnect path latches the in-flight turn's outcome on the
// process, for a caller wired up minutes later. This file is that caller.
//
// The verdict comes from the router as a capability, not a SessionRouter
// method: CostReporter set the precedent — one production implementation
// gains it, twenty test fakes degrade to "nothing to adopt", which is
// exactly the pre-adoption behaviour and therefore the right default.

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// AdoptVerdict mirrors session.AdoptState value-for-value and
// ordinal-for-ordinal (the wireup adapter casts numerically; it panics at
// init on divergence, the same discipline InterruptOutcome uses).
type AdoptVerdict int

const (
	AdoptNone AdoptVerdict = iota
	AdoptLive
	AdoptDriftShutdown
)

// AdoptedRunOutcome is how an adopted turn ended, in cron's own vocabulary so
// this package still imports neither session nor cli.
type AdoptedRunOutcome struct {
	// Completed: the CLI delivered a result frame (Text/SubType filled).
	// False: the CLI went away without one — interrupted after all.
	Completed bool
	Text      string
	SubType   string
	SessionID string
}

// InFlightRun is the narrow handle an adoption holds while it waits.
type InFlightRun interface {
	// AwaitAdopted blocks until the adopted turn resolves or ctx ends.
	AwaitAdopted(ctx context.Context) (AdoptedRunOutcome, error)
}

// InFlightAdopter is asserted on the SessionRouter, never added to it.
type InFlightAdopter interface {
	AdoptInFlight(key string) (InFlightRun, AdoptVerdict)
}

// maxAdoptAttempts bounds how many boots may try to adopt one marker. Phase 0
// removed markers before recording precisely so a bad marker could not be
// re-read every boot; adoption needs the marker to survive INTO the attempt,
// so the bound moves into the marker instead of disappearing. One attempt:
// the worst case is one extra boot, never a crash loop (#2751's class).
const maxAdoptAttempts = 1

// adoptionWaitBudget caps how long an adoption waits for the late result.
// The generous default covers a long turn that was near its own deadline when
// the restart hit; the run's own JobTimeout no longer applies (the timer died
// with the old process), so this is thestand-in  upper bound.
var adoptionWaitBudget = 30 * time.Minute

// adoptRun waits out one adopted turn and writes its terminal record. Runs in
// its own goroutine under goStartupPass (recover + gcWG). The gate slot was
// claimed by the caller; finishing releases it via the same finalizer every
// run body uses, so CurrentRun/overlap/gauge behave as if the run were local.
func (s *Scheduler) adoptRun(m runInflightMarker, run InFlightRun, inflight *runInflight) {
	startedAt := time.UnixMilli(m.StartedAtMS)
	finalizer := &runFinalizer{inflight: inflight}
	sc := runScaffold{finalizer: finalizer, jobID: m.JobID}
	sc.run(func() {
		ctx, cancel := context.WithTimeout(s.stopCtx, adoptionWaitBudget)
		defer cancel()
		out, err := run.AwaitAdopted(ctx)
		now := time.Now()
		rec := &CronRun{
			RunID:      m.RunID,
			JobID:      m.JobID,
			Trigger:    m.Trigger,
			StartedAt:  startedAt,
			EndedAt:    now,
			DurationMS: now.Sub(startedAt).Milliseconds(),
			Prompt:     osutil.SanitizeForLog(m.Prompt, MaxPromptBytes),
			WorkDir:    m.WorkDir,
			Fresh:      m.Fresh,
			SessionID:  out.SessionID,
			// CostUSD deliberately zero: the adopted session's baseline covers
			// turns this process never saw, and a wrong number in the ledger is
			// worse than none (#2750).
		}
		switch {
		case err == nil && out.Completed:
			rec.State = RunStateSucceeded
			rec.Result = out.Text
			rec.ResultBytes = len(out.Text)
		default:
			// Timeout, shutdown, or the CLI died without a result: the run is
			// interrupted after all — Phase 0's record, with a duration that now
			// spans the restart.
			rec.State = RunStateCanceled
			rec.ErrorClass = ErrClassInterrupted
			rec.ErrorMsg = "naozhi restarted mid-run; the adopted CLI turn did not complete"
		}
		if s.jobStillExists(m.JobID) {
			s.appendRun(rec)
		}
		s.removeRunInflightMarker(m.RunID)
		slog.Info("cron: adopted run settled",
			"job_id", m.JobID, "run_id", m.RunID, "state", rec.State,
			"spanned_restart", true)
	})
}
