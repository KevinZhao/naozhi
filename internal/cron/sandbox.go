package cron

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
)

// runtimeSessionIDRe matches the format produced by sandboxRuntimeSessionID:
// "run-<lowercase-hex>-<decimal-unixnano>". Validates RuntimeSessionID values
// read from operator-writable disk files before StopSession (#2065).
var runtimeSessionIDRe = regexp.MustCompile(`^run-[0-9a-f]+-[0-9]+$`)

// isValidRuntimeSessionID reports whether s matches the production format of
// a sandbox runtime session id; called before every StopSession whose id came
// from disk. The 128-byte cap rejects pathologically long strings.
func isValidRuntimeSessionID(s string) bool {
	return len(s) <= 128 && runtimeSessionIDRe.MatchString(s)
}

// SandboxJob is the run-once unit handed to the sandbox placement
// (agentcore-cloud-sandbox RFC §3.1). One job = one microVM = one prompt;
// no resume, no reattach.
type SandboxJob struct {
	JobID  string
	RunID  string
	Prompt string
	// Model pins the CLI model inside the microVM ("" = image default).
	Model string
	// RuntimeSessionID is the platform session id for this run. Derived by cron
	// (not the adapter) because the pending record must hold it BEFORE the invoke
	// — it is the only handle a restarted naozhi has to Stop an orphaned microVM.
	// Unique per run and ≥33 chars: "run-<cronRunID>-<unixnano>".
	RuntimeSessionID string
}

// sandboxRuntimeSessionID derives the platform session id for one run.
// Embeds the cron runID so CloudTrail / platform logs correlate back to
// the run record; the nano suffix guarantees uniqueness even across a
// hypothetical runID collision and pads past the 33-char API minimum.
func sandboxRuntimeSessionID(runID string, startedAt time.Time) string {
	return fmt.Sprintf("run-%s-%d", runID, startedAt.UnixNano())
}

// Sandbox terminal states, mirroring agentcore.TerminalState wire values.
// cron re-declares the strings instead of importing internal/agentcore so the
// scheduler stays compile-time independent of the AWS SDK.
const (
	SandboxStateSuccess         = "success"
	SandboxStateFailedClean     = "failed-clean"
	SandboxStateFailedTransport = "failed-transport"
)

// SandboxOutcome reports how a sandbox run ended.
type SandboxOutcome struct {
	// State is one of the SandboxState* values above.
	State string
	// ResultText is the CLI's final result text (success path; may be the
	// error text on failed-clean).
	ResultText string
	// ErrMsg is the human-readable failure detail ("" on success).
	ErrMsg string
	// StopConfirmed reports whether StopRuntimeSession was confirmed after a
	// transport failure. Only meaningful for SandboxStateFailedTransport: false
	// means the microVM's fate is UNKNOWN and replay must refuse to act until a
	// Stop succeeds.
	StopConfirmed bool
	// Meta is the per-run execution receipt (cost / memory peak / image / exit)
	// surfaced into the run record; zero-valued fields render as "unknown". The
	// wireup adapter populates it from agentcore.RunResult.
	Meta SandboxRunMeta
}

// SandboxRunMeta is the cloud-execution receipt for one sandbox run. cron
// re-declares it so the scheduler stays independent of the AWS SDK; the wireup
// adapter maps agentcore.RunResult → this struct. Every field omitempty so a
// partial receipt persists only what it knows. NO secrets, NO AWS-internal IDs.
type SandboxRunMeta struct {
	RuntimeARN   string `json:"runtime_arn,omitempty"`
	ImageVersion string `json:"image_version,omitempty"`
	// ExitStatus has NO omitempty: exit 0 is the meaningful "success" value and a
	// missing key would be indistinguishable from "exit unknown". The enclosing
	// *SandboxRunMeta is itself omitempty, so local runs carry no exit_status.
	ExitStatus      int     `json:"exit_status"`
	CostUSD         float64 `json:"cost_usd,omitempty"`
	DurationMS      int64   `json:"duration_ms,omitempty"`
	MemoryPeakBytes int64   `json:"memory_peak_bytes,omitempty"`
	// Models / Basis are the CLI result's per-model drill-down and worst
	// price basis, carried into the ledger receipt.
	Models []costledger.ModelDelta `json:"models,omitempty"`
	Basis  costledger.Basis        `json:"basis,omitempty"`
}

// isZero reports whether the receipt carries no information (every field
// at its zero value) — used to decide whether to attach it to the run
// record at all, so non-sandbox runs never grow a `sandbox_meta` key.
func (m SandboxRunMeta) isZero() bool {
	return m.RuntimeARN == "" && m.ImageVersion == "" && m.ExitStatus == 0 && m.CostUSD == 0 &&
		m.DurationMS == 0 && m.MemoryPeakBytes == 0 && len(m.Models) == 0 && m.Basis == ""
}

// SandboxRunner executes run-once jobs at the sandbox placement. The
// production implementation (wireup) wraps agentcore.Client; nil deps route
// sandbox jobs to the ErrClassSandboxUnavailable failure path.
//
// eventSink receives every decoded stream envelope as one raw JSON line, in
// order, from the goroutine that owns the stream; cron persists them without
// understanding the schema. The cron-provided sink never returns an error (a
// naozhi-side disk fault must not look like a transport break and Stop a
// healthy microVM); the error return exists for the agentcore client contract.
type SandboxRunner interface {
	RunJob(ctx context.Context, job SandboxJob, eventSink func(line []byte) error) (SandboxOutcome, error)
	// StopSession terminates a runtime session by its platform id. Idempotent
	// server-side; callers treat an error as "fate unknown" and surface it.
	StopSession(ctx context.Context, runtimeSessionID string) error
}

// sandboxMaxRunDuration is the wall-clock fence: the streaming connection caps
// at 60min and the runtime's maxLifetime is clamped to the same bound so a job
// cannot outlive a cut stream. Effective budget is min(execTimeout, this).
const sandboxMaxRunDuration = 60 * time.Minute

// sandboxRunBudget is a sandbox run's wall-clock budget: execTimeout capped at
// sandboxMaxRunDuration.
func (s *Scheduler) sandboxRunBudget() time.Duration {
	if s.execTimeout <= 0 || s.execTimeout > sandboxMaxRunDuration {
		return sandboxMaxRunDuration
	}
	return s.execTimeout
}

// sandboxExecArgs carries the executeOpt-owned state into the sandbox branch.
// The run's identity comes from the embedded runCtx (Epic H #2546); what stays
// here is what only this branch needs.
type sandboxExecArgs struct {
	runCtx
	prompt string // agent-command-stripped prompt (cleanText)
	model  string // resolved agent model ("" = image default)
	// replayOf links this run to the original it re-executes; "" for a normal
	// run. Set by ReplaySandboxRun, threaded to CronRun.ReplayOf.
	replayOf string
}

// executeSandbox runs one cron job at the sandbox placement and routes the
// outcome through the same finishRun terminal protocol as local runs. It owns
// no session-router state (no GetOrCreate / Reset / stubs): the microVM burns
// on completion.
//
// Restart immunity: a pending record (sandbox_pending.go) is written before
// the invoke and removed after terminal state; startup reconcile Stops
// orphans. Delete immunity: DeleteJobByID Stops the microVM via
// stopSandboxRunsForJob; this goroutine still reaches finishRun, which no-ops
// the persist for the deleted job.
func (s *Scheduler) executeSandbox(a sandboxExecArgs) {
	a.lg.Info("cron job executing in sandbox", "prompt_len", len(a.prompt))

	// No workspace at sandbox placement (clone-on-boot not implemented). Reject at
	// run time too so a job edited into this shape by a non-dashboard caller fails
	// loudly instead of running CC in an empty directory.
	if a.snap.workDir != "" {
		// SandboxFailed (job misconfiguration), NOT Unavailable: alerting on
		// sandbox_unavailable must mean "wire the config".
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, "",
			"sandbox placement does not support work_dir (Phase 1; use placement=local)", nil)
		return
	}
	if s.sandbox == nil {
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxUnavailable, "",
			"sandbox placement not configured (cron.sandbox in config)", nil)
		return
	}

	a.inflight.setPhase(PhaseSending)

	ctx, cancel := context.WithTimeout(s.stopCtx, s.sandboxRunBudget())
	defer cancel()

	// Pending record persisted BEFORE the invoke so a restart mid-hold can Stop the
	// orphaned microVM and close the run. Best-effort: a write failure only loses
	// restart immunity (orphan bounded by maxLifetime), it does not fail the run.
	runtimeSID := sandboxRuntimeSessionID(a.runID, a.startedAt)
	pendingPath := s.writeSandboxPending(sandboxstore.Pending{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		RuntimeSessionID: runtimeSID,
		StartedAtMS:      a.startedAt.UnixMilli(),
	}, a.lg)

	// Input snapshot (content-addressed prompt + model) persisted BEFORE the invoke
	// so a replay re-injects the exact payload. No secrets are injected yet, and
	// the image version is unknown until the run reports it. Best-effort.
	s.sandboxState().WriteSnapshot(a.snap.jobID, a.runID, a.prompt, a.model, "", nil, a.lg)

	sink, closeSink := s.sandboxState().EventSink(a.snap.jobID, a.runID, a.lg)
	// RunJob can panic inside SDK/streaming code and skip the ordered closeSink()
	// below, leaking the event-log fd; closeSink is idempotent so this defer is a
	// safe fallback (#2317).
	defer closeSink()
	outcome, err := s.sandbox.RunJob(ctx, SandboxJob{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		Prompt:           a.prompt,
		Model:            a.model,
		RuntimeSessionID: runtimeSID,
	}, sink)
	// Flush the event log BEFORE finishRun broadcasts the terminal frame so a
	// dashboard client reacting to RunEnded finds the complete log on disk.
	closeSink()
	if err != nil {
		// Pre-flight failure: the job never reached the platform, so the pending
		// handle is moot.
		s.removeSandboxPending(pendingPath, a.lg)
		s.clearSandboxPendingIndex(a.snap.jobID, pendingPath)
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, "",
			"sandbox preflight: "+sanitiseRunErrMsg(err.Error()), nil)
		return
	}

	// nil when the receipt is entirely empty so a degenerate run never grows a
	// sandbox_meta key.
	metaPtr := sandboxMetaPtr(outcome.Meta)

	switch outcome.State {
	case SandboxStateSuccess:
		s.removeSandboxPending(pendingPath, a.lg)
		s.clearSandboxPendingIndex(a.snap.jobID, pendingPath)
		s.finishSandboxRun(a, RunStateSucceeded, ErrClassNone, outcome.ResultText, "", metaPtr)
	case SandboxStateFailedClean:
		s.removeSandboxPending(pendingPath, a.lg)
		s.clearSandboxPendingIndex(a.snap.jobID, pendingPath)
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, outcome.ResultText,
			sanitiseRunErrMsg(outcome.ErrMsg), metaPtr)
	default: // SandboxStateFailedTransport and any future unknown state: conservative.
		// The runner already attempted StopRuntimeSession; record whether it was
		// confirmed for the confirmation queue and operators reading history.
		msg := "sandbox stream lost before terminal attestation"
		if outcome.ErrMsg != "" {
			msg = sanitiseRunErrMsg(outcome.ErrMsg)
		}
		if outcome.StopConfirmed {
			// §6.2 rule 1 satisfied in-process — the retry handle is spent.
			s.removeSandboxPending(pendingPath, a.lg)
			s.clearSandboxPendingIndex(a.snap.jobID, pendingPath)
			msg += " (microVM termination confirmed)"
		} else {
			// Stop unconfirmed: KEEP the pending file so startup reconcile retries
			// StopSession — removing it would discard the only retry handle for a microVM
			// whose fate is unknown.
			a.lg.Warn("cron sandbox: termination unconfirmed; pending record kept for startup reconcile",
				"pending", pendingPath != "")
			msg += " (microVM fate UNKNOWN — termination unconfirmed; check for side effects before re-running)"
		}
		// Shutdown cancel (s.stopCtx → ctx.Err()==Canceled) is RunStateCanceled with
		// skipPersist, matching the local path — a graceful shutdown is not a
		// transport failure (#2059). Only genuine transport failures (DeadlineExceeded
		// / default) feed the human confirmation queue: a cancelled run enqueued for
		// attention would leave a phantom "needs confirm" entry (#2081).
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			s.finishSandboxRunSkipPersist(a, RunStateCanceled, ErrClassCanceled, outcome.ResultText,
				"sandbox run canceled by shutdown", metaPtr)
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.enqueueSandboxTransportAttention(a, runtimeSID)
			s.finishSandboxRun(a, RunStateTimedOut, ErrClassSandboxTransport, outcome.ResultText, msg, metaPtr)
		default:
			s.enqueueSandboxTransportAttention(a, runtimeSID)
			s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxTransport, outcome.ResultText, msg, metaPtr)
		}
	}
}

// enqueueSandboxTransportAttention adds a side-effecting job's genuine
// transport failure to the human confirmation queue: the operator checks
// whether the side effect already landed before confirm-done or replay. A
// side-effect-free job never enters the queue. RuntimeSessionID is carried so
// replay can Stop before re-running. Written regardless of StopConfirmed —
// the side effect may have landed before the stream broke.
//
// Callers MUST NOT invoke this for shutdown-cancel runs (#2081).
func (s *Scheduler) enqueueSandboxTransportAttention(a sandboxExecArgs, runtimeSID string) {
	if !a.snap.sideEffects {
		return
	}
	// A concurrent DeleteJobByID may already have cleared this job's attention
	// queue while this goroutine was blocked on the severed stream; writing now
	// would leave a phantom queue card whose replay ErrJobNotFound's. Re-check
	// s.tbl.jobs[id] (mirrors recordTerminalResult) and skip if the job is gone.
	if !s.tbl.exists(a.snap.jobID) {
		a.lg.Info("cron sandbox: transport-attention skipped — job deleted mid-flight (R20260614-ARCH-1)",
			"job_id", a.snap.jobID, "run_id", a.runID)
		return
	}
	s.sandboxState().WriteAttention(sandboxstore.Attention{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		RuntimeSessionID: runtimeSID,
		Reason:           sandboxstore.ReasonTransport,
		JobLabel:         a.snap.label,
		StartedAtMS:      a.startedAt.UnixMilli(),
		CreatedAtMS:      s.attentionNowMS(),
	}, a.lg)
}

// sandboxMetaPtr returns &meta when the receipt carries any information,
// else nil — so a run that produced no receipt (preflight failure,
// unavailable executor) never grows a sandbox_meta key in its record.
func sandboxMetaPtr(meta SandboxRunMeta) *SandboxRunMeta {
	if meta.isZero() {
		return nil
	}
	m := meta
	return &m
}

// finishSandboxRun funnels every sandbox terminal path through finishRun
// (same three-write protocol as local runs: persist → metrics → broadcast)
// plus the completion notice. meta is the cloud-execution receipt (nil for
// pre-invoke failures that produced no receipt).
func (s *Scheduler) finishSandboxRun(a sandboxExecArgs, state RunState, errClass ErrorClass, result, errMsg string, meta *SandboxRunMeta) {
	s.finishSandboxRunWith(a, state, errClass, result, errMsg, meta, false)
}

// finishSandboxRunSkipPersist is the shutdown-cancel variant of
// finishSandboxRun: skipPersist keeps the canceled run out of Job state and
// run history, mirroring the local path; the WS broadcast still fires (#2059).
func (s *Scheduler) finishSandboxRunSkipPersist(a sandboxExecArgs, state RunState, errClass ErrorClass, result, errMsg string, meta *SandboxRunMeta) {
	s.finishSandboxRunWith(a, state, errClass, result, errMsg, meta, true)
}

func (s *Scheduler) finishSandboxRunWith(a sandboxExecArgs, state RunState, errClass ErrorClass, result, errMsg string, meta *SandboxRunMeta, skipPersist bool) {
	if state == RunStateSucceeded {
		s.observeSuccessLatency(s.now().Sub(a.startedAt), SendResult{Text: result}, a.snap, a.lg)
	} else if state == RunStateCanceled {
		a.lg.Info("cron sandbox run canceled",
			"err_class", string(errClass), "err", errMsg)
	} else if state == RunStateFailed {
		a.lg.Error("cron sandbox run failed",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	} else if state == RunStateTimedOut {
		a.lg.Error("cron sandbox run timed out",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	} else {
		a.lg.Info("cron sandbox run ended with non-failure terminal state",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	}
	// No metrics here: finishRun → bumpRunStateMetrics(state, sandbox=true) is the
	// single owner of every per-state counter, and the state already encodes the
	// TimedOut-vs-Failed split so a timed-out run is never counted twice (#2173).
	paused := s.finishRun(a.runCtx, runOutcome{
		state: state, errClass: errClass, errMsg: errMsg, result: result,
		skipPersist: skipPersist,
		sandboxMeta: meta,
		replayOf:    a.replayOf,
		sandbox:     true,
	})
	switch state {
	case RunStateCanceled:
		// A shutdown-cancel is not a user-visible failure — no notice, mirroring
		// the local path (#2059).
	case RunStateSucceeded:
		// Same pipeline as the local success path: sanitise then localize API-error
		// envelopes before anything reaches IM.
		s.deliverNotice(a.notifyTo, formatCronNotice(a.snap.labelOrID(), localizeNotice(result)))
	default:
		s.deliverFailureNotice(a.runCtx, errClass, state, s.sandboxRunBudget(), paused)
	}
}
