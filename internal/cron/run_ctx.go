package cron

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// run_ctx.go — the identity a run carries from admission to terminal state, and
// the outcome it ends with. Epic H #2546, direction 1.
//
// Five *Args structs bundle the inputs of the execution phases (preflightArgs,
// getSessionArgs, execSendArgs, sandboxExecArgs, finishArgs). Measured, they
// share {job, runID, startedAt, trigger, finalizer} in all five and
// {snap, lg, notifyTo, key, inflight} in three or more — the same block spelled
// five times, and then spelled AGAIN at each of the fourteen
// finishRun(finishArgs{...}) call sites, where eleven of the twenty-one fields
// appear in thirteen or fourteen of them.
//
// runCtx is that block, built once per run in executeAcquired. runOutcome is the
// situational remainder. finishRunFor composes the two, so a terminal branch
// spells only what is specific to it:
//
//	s.finishRunFor(a.runCtx, runOutcome{state: RunStateCanceled,
//	    errClass: ErrClassCanceled, errMsg: err.Error(), skipPersist: true})
//
// finishRun(finishArgs) is deliberately left in place rather than reshaped:
// thirty-seven test call sites across eighteen files construct finishArgs
// directly, and two production callers are not on the execution path at all
// (sandbox_pending.go's restart reconciler and scheduler_finish.go's synthetic
// skip), so they have no runCtx to pass. Funnelling those two would force
// unrelated paths through one door for the sake of a count.

// runCtx is one run's identity and context: everything a terminal branch needs
// that does not depend on HOW the run ended.
type runCtx struct {
	// snap is the jobSnapshot taken at admission. Phases read snap rather than
	// *job so a concurrent DeleteJob/PauseJob cannot race them.
	snap jobSnapshot
	// startedAt is the wall-clock start recorded on entry to executeOpt. Failure
	// branches keep it rather than re-reading the clock, so the dashboard shows
	// the real trigger-to-giving-up duration.
	startedAt time.Time
	// notifyTo is the resolved IM target. Only some branches notify.
	notifyTo NotifyTarget
	// key is the router session key (`cron:<jobID>`).
	key string
	// runID pairs the terminal event with the started event already broadcast.
	runID string
	// trigger must match the RunStartedEvent's.
	trigger TriggerKind
	// job is the run's Job. Terminal branches pass it through; phases do not
	// mutate it.
	job *Job
	// lg is the per-run logger, already tagged with jobID/runID.
	lg *slog.Logger
	// finalizer releases the inflight CAS gate. finishRun calls it before the
	// terminal broadcast so CurrentRun and the broadcast agree.
	finalizer *runFinalizer
	// inflight is the per-run gate slot; phases stamp progress on it.
	inflight *runInflight
}

// runOutcome is how a run ended: the fields that vary per terminal branch.
// The zero value plus a state is a complete outcome.
type runOutcome struct {
	state    RunState
	errClass ErrorClass
	errMsg   string
	// result is the CLI's final text, already sanitised by the caller where the
	// path produces one.
	result string
	// sessionID is the CLI session_id; empty on fresh-context and failure paths.
	sessionID string
	// skipPersist keeps a transient terminal (canceled / overlap / deleted
	// mid-execute) out of Job state and runs/ history. Metrics and the WS
	// broadcast still fire.
	skipPersist bool
	// keepInflightMarker survives this finish: set ONLY by the shutdown-cancel
	// paths, where the Send's ctx died because the PROCESS is going away while
	// the CLI keeps running behind its shim. The marker's claim — "this run
	// never finished" — is still true then, and deleting it would disinherit
	// the next process's adoption pass (#2712 PR B): the run would vanish
	// instead of completing across the restart. Every other terminal state
	// still clears the marker.
	keepInflightMarker bool
	// costInc is a local run's spend; sandbox runs carry cost via sandboxMeta.
	costInc costledger.Increment
	// endedAt, when non-zero, overrides finishRun's own clock read so a caller
	// that already read it (observeSuccessLatency) shares the reading.
	endedAt time.Time
	// sandboxMeta is the cloud-execution receipt; nil for local runs.
	sandboxMeta *SandboxRunMeta
	// sandbox marks a placement=sandbox run even when no receipt exists yet
	// (pre-invoke failures), so the sandbox metric buckets stay a strict subset.
	sandbox bool
	// replayOf links this run to the one it re-executes; "" normally.
	replayOf string
}

// finishRunFor is the single composition point from (identity, outcome) to
// finishRun. Snapshot fields come from rc.snap, which is the zero value on paths
// that never reached snapshotJob — exactly the "prompt/workDir/fresh are zero
// pre-snapshot" contract finishArgs already documents.
func (s *Scheduler) finishRunFor(rc runCtx, out runOutcome) {
	s.finishRun(finishArgs{
		job:                rc.job,
		runID:              rc.runID,
		startedAt:          rc.startedAt,
		trigger:            rc.trigger,
		finalizer:          rc.finalizer,
		prompt:             rc.snap.prompt,
		workDir:            rc.snap.workDir,
		fresh:              rc.snap.fresh,
		state:              out.state,
		errClass:           out.errClass,
		errMsg:             out.errMsg,
		result:             out.result,
		sessionID:          out.sessionID,
		skipPersist:        out.skipPersist,
		keepInflightMarker: out.keepInflightMarker,
		costInc:            out.costInc,
		endedAt:            out.endedAt,
		sandboxMeta:        out.sandboxMeta,
		sandbox:            out.sandbox,
		replayOf:           out.replayOf,
	})
}
