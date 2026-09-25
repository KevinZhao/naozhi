package cron

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// run_ctx.go — the identity a run carries from admission to terminal state, and
// the outcome it ends with. Epic H #2546, direction 1.
//
// The execution-phase argument structs (preflightArgs, getSessionArgs,
// execSendArgs, sandboxExecArgs) all carry {job, runID, startedAt, trigger,
// finalizer}, and most also {snap, lg, notifyTo, key, inflight}. That shared
// block is runCtx, embedded rather than re-spelled per phase.
//
// runCtx is that block, built once per run in executeAcquired; runOutcome is
// the situational remainder. finishRun takes the two directly, so a terminal
// branch spells only what is specific to it:
//
//	s.finishRun(a.runCtx, runOutcome{state: RunStateCanceled,
//	    errClass: ErrClassCanceled, errMsg: err.Error(), skipPersist: true})
//
// The two callers that are not on the execution path — the restart reconciler
// (sandbox_pending.go) and the synthetic skip (scheduler_finish.go) — build a
// runCtx holding only the identity they have. A zero snap there is the same
// "prompt/workDir/fresh are zero before snapshotJob" contract every
// pre-snapshot failure branch already relies on; the dashboard falls back to
// Job.Prompt for display.

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
	// runID pairs the terminal event with the started event already broadcast;
	// the dashboard hub matches started→ended frames on it.
	runID string
	// trigger must match the RunStartedEvent's.
	trigger TriggerKind
	// job is the run's Job. Terminal branches pass it through; phases do not
	// mutate it. Required even on the overlap-skip path, because emitRunEnded
	// keys the event by Job.ID; a DeleteJob racing the finish is caught by the
	// jobs[id] re-check inside recordTerminalResult.
	job *Job
	// lg is the per-run logger, already tagged with jobID/runID.
	lg *slog.Logger
	// finalizer releases the inflight CAS gate. finishRun calls it before the
	// terminal broadcast so CurrentRun and the broadcast agree; the caller's
	// defer calls it again as a backstop, and its done flag makes that
	// idempotent and scoped to THIS run's gate. nil means "this finish owns no
	// gate": the overlap-skip and synthetic-skip paths must pass nil, because
	// the gate they would name belongs to the concurrent run they were skipped
	// for. The restart reconciler passes an EMPTY non-nil finalizer for the
	// same reason (the orphan's gate died with the previous process).
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
	// errClass is the machine-readable class the dashboard picks its icon and
	// i18n copy by; errMsg is only the expanded detail — human-readable, control
	// characters escaped, absolute paths redacted, ≤ maxCronErrMsgRunes.
	// result is the CLI's final text, already through sanitiseRunResult (4K
	// rune cap + truncation suffix + control-character filter) where the path
	// produces one.
	result string
	// sessionID is the CLI session_id; empty on fresh-context and failure
	// paths, which hides the dashboard's open-session button.
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
	// (pre-invoke failures), so bumpRunStateMetrics also advances the
	// CronSandboxRun{Failed,TimedOut}Total buckets and they stay a strict
	// subset of the run totals (#2173). Deliberately separate from
	// sandboxMeta != nil for that reason.
	sandbox bool
	// replayOf links this run to the one it re-executes; "" normally.
	replayOf string
}
