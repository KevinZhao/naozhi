package cron

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/naozhi/naozhi/internal/agentcore"
	"github.com/naozhi/naozhi/internal/metrics"
)

// runtimeSessionIDRe matches the production format produced by
// sandboxRuntimeSessionID: "run-<lowercase-hex>-<decimal-unixnano>".
// Used to validate RuntimeSessionID values read from operator-writable
// disk files before passing them to StopSession. [R20260613-SEC-2 / #2065]
var runtimeSessionIDRe = regexp.MustCompile(`^run-[0-9a-f]+-[0-9]+$`)

// isValidRuntimeSessionID reports whether s matches the expected production
// format of a sandbox runtime session id. Called before every StopSession
// invocation whose session id was read from disk (sandbox_pending.go /
// sandbox_replay.go). Invalid ids are logged and skipped.
//
// The length cap (128 bytes) is generous vs the ~40-char production value
// ("run-<16-hex>-<19-digit-nano>") and rejects pathologically long strings
// that could not be legitimate session ids.
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
	// RuntimeSessionID is the platform session id for this run. Derived by
	// cron (not the adapter) because the §6.5 pending record must hold it
	// BEFORE the invoke is attempted — it is the only handle a restarted
	// naozhi has to Stop an orphaned microVM. Unique per run (§4.1) and
	// ≥33 chars (validation F3): "run-<cronRunID>-<unixnano>".
	RuntimeSessionID string
}

// sandboxRuntimeSessionID derives the platform session id for one run.
// Embeds the cron runID so CloudTrail / platform logs correlate back to
// the run record; the nano suffix guarantees uniqueness even across a
// hypothetical runID collision and pads past the 33-char API minimum.
func sandboxRuntimeSessionID(runID string, startedAt time.Time) string {
	return fmt.Sprintf("run-%s-%d", runID, startedAt.UnixNano())
}

// Sandbox terminal states, mirroring agentcore.TerminalState wire values
// (RFC §6.1). cron re-declares the strings instead of importing
// internal/agentcore so the scheduler stays compile-time independent of
// the AWS SDK — the wireup layer owns that edge (same reasoning as
// SessionRouter / NotifySender seams in deps.go).
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
	// StopConfirmed reports whether the §6.2 rule-1 termination
	// (StopRuntimeSession) was confirmed after a transport failure. Only
	// meaningful when State == SandboxStateFailedTransport: false means
	// the microVM's fate is UNKNOWN and any replay machinery must refuse
	// to act on this run until a Stop succeeds.
	StopConfirmed bool
	// Meta is the per-run execution receipt (cost / memory peak / image /
	// exit) surfaced into the run record (RFC §7.3/§7.5). Zero-valued
	// fields render as "unknown" — a transport failure with no result
	// event carries no cost, an old image carries no version. The adapter
	// (wireup) populates this from agentcore.RunResult.
	Meta SandboxRunMeta
}

// SandboxRunMeta is the cloud-execution receipt for one sandbox run
// (RFC §5.1 meta block). cron re-declares it (rather than importing an
// agentcore type) so the scheduler stays compile-time independent of the
// AWS SDK — the wireup adapter maps agentcore.RunResult → this struct.
// Every field omitempty: a partial receipt (transport failure: cost/exit
// unknown) persists only what it knows. NO secrets, NO AWS-internal IDs.
type SandboxRunMeta struct {
	RuntimeARN   string `json:"runtime_arn,omitempty"`
	ImageVersion string `json:"image_version,omitempty"`
	// ExitStatus has NO omitempty: exit 0 is the meaningful "success"
	// value, and a missing key would be indistinguishable from "exit
	// unknown" (transport failure). The enclosing *SandboxRunMeta is
	// itself omitempty, so local runs still carry no exit_status at all —
	// only attested sandbox runs record it, and they record it always.
	ExitStatus      int     `json:"exit_status"`
	CostUSD         float64 `json:"cost_usd,omitempty"`
	DurationMS      int64   `json:"duration_ms,omitempty"`
	MemoryPeakBytes int64   `json:"memory_peak_bytes,omitempty"`
}

// isZero reports whether the receipt carries no information (every field
// at its zero value) — used to decide whether to attach it to the run
// record at all, so non-sandbox runs never grow a `sandbox_meta` key.
func (m SandboxRunMeta) isZero() bool {
	return m == SandboxRunMeta{}
}

// SandboxRunner executes run-once jobs at the sandbox placement. The
// production implementation (wireup) wraps agentcore.Client: payload
// construction, the held event stream, terminal classification, and the
// transport-failure Stop confirmation all live behind this seam. nil deps
// (or a disabled config) leave the scheduler routing sandbox jobs to the
// ErrClassSandboxUnavailable failure path.
//
// eventSink receives every decoded stream envelope as one raw JSON line,
// in order, from the goroutine that owns the stream — the cron side
// persists them (run-record seed, RFC §6.1 streaming-to-disk requirement)
// without understanding the envelope schema. The cron-provided sink never
// returns an error (write failures degrade to a logged no-op inside
// sandboxEventSink — a naozhi-side disk fault must not look like a
// transport break and Stop a healthy microVM); the error return exists
// for the agentcore client's contract and future sinks that genuinely
// cannot continue.
type SandboxRunner interface {
	RunJob(ctx context.Context, job SandboxJob, eventSink func(line []byte) error) (SandboxOutcome, error)
	// StopSession terminates a runtime session by its platform id — the
	// §6.2 rule-1 / §6.5 reconcile primitive. Idempotent server-side;
	// callers treat an error as "fate unknown" and surface it.
	StopSession(ctx context.Context, runtimeSessionID string) error
}

// sandboxMaxRunDuration is the Phase 1 wall-clock fence (RFC §6.2 rule 2):
// the A1-a streaming connection caps at 60min, and the runtime's
// maxLifetime is clamped to the same bound so a job cannot outlive a cut
// stream by hours. The effective budget is min(execTimeout, this).
const sandboxMaxRunDuration = 60 * time.Minute

// sandboxExecArgs carries the executeOpt-owned state into the sandbox
// branch. Mirrors the getSessionArgs/finishArgs bundling style.
type sandboxExecArgs struct {
	job       *Job
	snap      jobSnapshot
	runID     string
	startedAt time.Time
	trigger   TriggerKind
	prompt    string // agent-command-stripped prompt (cleanText)
	model     string // resolved agent model ("" = image default)
	notifyTo  NotifyTarget
	inflight  *runInflight
	finalizer *runFinalizer
	lg        *slog.Logger
	// replayOf links this run to the original run it re-executes (RFC §7.3).
	// "" for a normal scheduled/manual run; set by ReplaySandboxRun so the
	// new run's record carries the replay chain. Threaded through to
	// finishSandboxRun → finishRun → CronRun.ReplayOf.
	replayOf string
}

// executeSandbox runs one cron job at the sandbox placement and routes the
// outcome through the same finishRun terminal protocol as local runs. It
// owns no session-router state: no GetOrCreate, no Reset, no stubs — the
// microVM burns on completion, which is the whole point (RFC §3.3:
// structural elimination of the cron session leak).
//
// §6.5 restart immunity: an in-flight record (sandboxpending/<run>.json,
// see sandbox_pending.go) is written before the invoke and removed after
// terminal state; startup reconcile Stops orphans and closes their run
// records as failed-transport.
//
// Delete immunity (§6.2): DeleteJobByID of a job with an in-flight sandbox
// run now Stops the microVM via stopSandboxRunsForJob (deleteJobPostCleanup),
// using the runtime session id in the pending record. The run's own
// goroutine still reaches finishRun, which no-ops the persist for the
// now-deleted job via recordTerminalResult's jobs[id] re-check.
func (s *Scheduler) executeSandbox(a sandboxExecArgs) {
	a.lg.Info("cron job executing in sandbox", "prompt_len", len(a.prompt))

	// Phase 1 guardrails (RFC §12): no workspace at sandbox placement —
	// clone-on-boot is Phase 1.5 (B10-a). Reject at run time too (the
	// dashboard validates on save) so a job edited into this shape by a
	// non-dashboard caller fails loudly instead of running CC in an empty
	// directory the operator thinks is their repo.
	if a.snap.workDir != "" {
		// ErrClassSandboxFailed (job-level misconfiguration), NOT
		// Unavailable — the executor may be perfectly healthy; alerting
		// keyed on sandbox_unavailable must mean "wire the config".
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

	budget := s.execTimeout
	if budget <= 0 || budget > sandboxMaxRunDuration {
		budget = sandboxMaxRunDuration
	}
	ctx, cancel := context.WithTimeout(s.stopCtx, budget)
	defer cancel()

	// §6.5 in-flight record: persist {job, run, runtime session, started}
	// BEFORE the invoke. If naozhi restarts mid-hold, startup reconcile
	// finds this file, Stops the orphaned microVM, and closes the run.
	// Best-effort: a write failure degrades to the pre-§6.5 behaviour
	// (orphan bounded by maxLifetime) rather than failing the run.
	runtimeSID := sandboxRuntimeSessionID(a.runID, a.startedAt)
	pendingPath := s.writeSandboxPending(sandboxPending{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		RuntimeSessionID: runtimeSID,
		StartedAtMS:      a.startedAt.UnixMilli(),
	}, a.lg)

	// §5.1/§5.2 input snapshot: persist the run's INPUT (content-addressed
	// prompt + model) BEFORE the invoke so a replay re-injects the exact
	// payload. Phase 1 has no injected secrets, so SecretRefs is empty; the
	// image version is unknown until the run reports it (the meta frame), so
	// it is "" here — replay falls back to the runtime's current image.
	// Best-effort (logs on failure, never fails the run).
	s.writeSandboxSnapshot(a.snap.jobID, a.runID, a.prompt, a.model, "", nil, a.lg)

	sink, closeSink := s.sandboxEventSink(a.snap.jobID, a.runID, a.lg)
	outcome, err := s.sandbox.RunJob(ctx, SandboxJob{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		Prompt:           a.prompt,
		Model:            a.model,
		RuntimeSessionID: runtimeSID,
	}, sink)
	// Close (flush) the event log BEFORE any finishRun below broadcasts the
	// terminal frame — a dashboard client reacting to RunEnded must find the
	// complete log on disk, not race a buffered tail (review PR-2b F1).
	closeSink()
	if err != nil {
		// Pre-flight failure: the job never reached the platform (invalid
		// payload — e.g. empty prompt). Permanent, not transport. The
		// microVM was never created, so the pending handle is moot.
		removeSandboxPending(pendingPath, a.lg)
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, "",
			"sandbox preflight: "+sanitiseRunErrMsg(err.Error()), nil)
		return
	}

	// Run-record receipt (RFC §7.3): meta the adapter filled from the
	// agentcore result. A transport failure may carry only partial meta
	// (no cost/exit) — still worth persisting what arrived. nil when the
	// receipt is entirely empty so a degenerate run never grows a
	// sandbox_meta key.
	metaPtr := sandboxMetaPtr(outcome.Meta)

	switch outcome.State {
	case SandboxStateSuccess:
		removeSandboxPending(pendingPath, a.lg)
		s.finishSandboxRun(a, RunStateSucceeded, ErrClassNone, outcome.ResultText, "", metaPtr)
	case SandboxStateFailedClean:
		removeSandboxPending(pendingPath, a.lg)
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, outcome.ResultText,
			sanitiseRunErrMsg(outcome.ErrMsg), metaPtr)
	default: // SandboxStateFailedTransport and any future unknown state: conservative.
		// §6.2 containment: the runner already attempted StopRuntimeSession;
		// surface whether the termination was confirmed. Phase 1 has no
		// auto-replay, so "do not replay before Stop confirms" holds
		// trivially — the flag is recorded for the Phase 3 confirmation
		// queue and for operators reading the run history today.
		msg := "sandbox stream lost before terminal attestation"
		if outcome.ErrMsg != "" {
			msg = sanitiseRunErrMsg(outcome.ErrMsg)
		}
		if outcome.StopConfirmed {
			// §6.2 rule 1 satisfied in-process — the retry handle is spent.
			removeSandboxPending(pendingPath, a.lg)
			msg += " (microVM termination confirmed)"
		} else {
			// Stop unconfirmed: KEEP the pending file. The next startup's
			// reconcile retries StopSession until it confirms — removing it
			// here would permanently discard the §6.2 retry handle for a
			// microVM whose fate is unknown (review §6.5 F2).
			a.lg.Warn("cron sandbox: termination unconfirmed; pending record kept for startup reconcile",
				"pending", pendingPath != "")
			msg += " (microVM fate UNKNOWN — termination unconfirmed; check for side effects before re-running)"
		}
		// R20260613-CR-6 (#2059): align shutdown-cancel classification with the
		// local path (scheduler_run.go). sandbox ctx = WithTimeout(s.stopCtx,
		// budget), so scheduler Stop cancels s.stopCtx → ctx.Err()=Canceled.
		// Treat that as RunStateCanceled with skipPersist=true (keep history
		// clean — a graceful shutdown is not a transport failure) rather than
		// recording a failed-transport run. DeadlineExceeded stays TimedOut.
		//
		// R20260613-LB-1 (#2081): the side-effecting human-confirmation-queue
		// write below MUST stay inside the non-cancel branches. A run cancelled
		// only by graceful shutdown is not a transport failure (it is recorded
		// as RunStateCanceled with skipPersist above's sibling branch); enqueuing
		// it for attention would leave the operator a phantom "needs confirm"
		// entry that a later reconcileSandboxPending overwrites with orphaned —
		// for a run whose history correctly reads Canceled. So only genuine
		// transport failures (DeadlineExceeded / default) feed the queue.
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
// transport failure to the human confirmation queue.
//
// §6.2 rule 3 + §7.4: a side-effecting job's transport failure must NOT
// auto-replay — it enters the human confirmation queue (the operator checks
// whether the side effect already landed before deciding to confirm-done or
// replay). A side-effect-free job is safe to re-run freely, so it never enters
// the queue (its failed-transport record still warns in history).
// RuntimeSessionID is carried so the queue's replay action can satisfy §6.2
// rule 1 (Stop before replay). The entry is written regardless of
// StopConfirmed: the microVM may be dead, but the SIDE EFFECT may still have
// landed before the stream broke, so a side-effecting job still needs a human
// look.
//
// R20260613-LB-1 (#2081): callers MUST NOT invoke this for shutdown-cancel
// (ctx.Err()==Canceled) runs — those are classified RunStateCanceled and kept
// out of the queue, so this is only reached from the DeadlineExceeded/default
// transport branches.
func (s *Scheduler) enqueueSandboxTransportAttention(a sandboxExecArgs, runtimeSID string) {
	if !a.snap.sideEffects {
		return
	}
	// R20260614-ARCH-1: a DeleteJobByID concurrent with this in-flight run can
	// reach deleteJobPostCleanup (stopSandboxRunsForJob → deleteJobRuns →
	// deleteJobAttention) while this goroutine is still blocked on the stream
	// that delete just severed; we then walk the sandbox.go default/timeout
	// branch into here AFTER deleteJobAttention already cleared the queue,
	// writing a ghost record for a job that no longer exists. ListSandboxAttention
	// only shape-validates the id (never job existence), so that record would
	// surface a phantom queue card whose replay ErrJobNotFound's. Re-check
	// s.jobs[id] under RLock (mirrors finishRun→recordTerminalResult's jobs[id]
	// re-check) and skip if the job is gone. The reconcile/test attention writers
	// stay on the unchecked writeSandboxAttention primitive: reconcile already
	// gates on j!=nil, and the test seam stages records for synthetic ids.
	s.mu.RLock()
	_, jobExists := s.jobs[a.snap.jobID]
	s.mu.RUnlock()
	if !jobExists {
		a.lg.Info("cron sandbox: transport-attention skipped — job deleted mid-flight (R20260614-ARCH-1)",
			"job_id", a.snap.jobID, "run_id", a.runID)
		return
	}
	s.writeSandboxAttention(sandboxAttention{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		RuntimeSessionID: runtimeSID,
		Reason:           attentionReasonTransport,
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

// finishSandboxRunSkipPersist is the shutdown-cancel variant (R20260613-CR-6 /
// #2059): like finishSandboxRun but sets finishArgs.skipPersist so the canceled
// run does not touch Job state (LastRunAt/LastResult) or grow a persisted
// failure record — mirroring the local path's shutdown-cancel handling
// (scheduler_run.go). The WS broadcast still fires so the dashboard sees the
// terminal frame.
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
		// R20260613-GOLANG-002: only count genuine failures as
		// CronSandboxRunFailedTotal. RunStateTimedOut is already counted by
		// bumpRunStateMetrics → CronRunTimedOutTotal inside finishRun; bumping
		// this counter too would double-count timed-out runs and pollute alerts.
		metrics.CronSandboxRunFailedTotal.Add(1)
		a.lg.Error("cron sandbox run failed",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	} else if state == RunStateTimedOut {
		// R20260614-LOGIC-9 (#2091): timed-out sandbox runs are deliberately
		// kept out of CronSandboxRunFailedTotal (avoid double-counting against
		// CronRunTimedOutTotal), but failure-only alerts would otherwise miss
		// sandbox deadlines entirely. A dedicated counter lets operators alert
		// on sandbox timeouts and isolate them from the path-mixed
		// CronRunTimedOutTotal.
		metrics.CronSandboxRunTimedOutTotal.Add(1)
		a.lg.Error("cron sandbox run timed out",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	} else {
		// Other non-success, non-canceled, non-failed terminal states: log at
		// Info so operators can see it without double-counting metrics.
		a.lg.Info("cron sandbox run ended with non-failure terminal state",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	}
	s.finishRun(finishArgs{
		job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
		state: state, errClass: errClass, errMsg: errMsg, result: result,
		skipPersist: skipPersist,
		prompt:      a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
		finalizer:   a.finalizer,
		sandboxMeta: meta,
		replayOf:    a.replayOf,
	})
	// R20260613-CR-6 (#2059): a shutdown-cancel is not a user-visible failure —
	// mirror the local path (scheduler_run.go), which delivers no notice when
	// the run is suppressed during shutdown.
	if state == RunStateCanceled {
		return
	}
	notice := "执行失败，请稍后重试。"
	if state == RunStateSucceeded {
		// Same pipeline as the local success path (R234-SEC-1 +
		// R20260531070014-ARCH-1): sanitise (truncate/redact) then localize
		// API-error envelopes before anything reaches an IM channel.
		notice = localizeNotice(result)
	} else if errClass == ErrClassSandboxTransport {
		notice = "云沙箱连接中断，任务状态未知，请检查执行历史。"
	}
	s.deliverNotice(a.notifyTo, formatCronNotice(a.snap.labelOrID(), notice))
}

// sandboxEventSink opens the per-run event log
// (<store-dir>/sandbox_events/<jobID>/<runID>.ndjson) and returns a sink
// writing one envelope per line, plus a closer. Streaming-to-disk is the
// §6.1 partial-result requirement: when the stream breaks mid-job, the
// events received so far are already durable. On open failure the sink
// degrades to a no-op (the run is more valuable than its event log) with
// one WARN.
//
// Phase 2's content-addressed run record (RFC §5) supersedes this layout;
// the directory is deliberately separate from the runStore's runs/ tree so
// the migration does not have to disentangle the two.
func (s *Scheduler) sandboxEventSink(jobID, runID string, lg *slog.Logger) (sink func([]byte) error, closer func()) {
	if s.storePath == "" {
		return func([]byte) error { return nil }, func() {}
	}
	dir := filepath.Join(filepath.Dir(s.storePath), "sandboxevents", jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		lg.Warn("cron sandbox: event log dir create failed; events not persisted", "err", err)
		return func([]byte) error { return nil }, func() {}
	}
	// runID is scheduler-generated hex (generateRunID), never user input,
	// so it is path-safe by construction; join defensively anyway.
	f, err := os.OpenFile(filepath.Join(dir, runID+".ndjson"),
		os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		lg.Warn("cron sandbox: event log open failed; events not persisted", "err", err)
		return func([]byte) error { return nil }, func() {}
	}
	w := bufio.NewWriterSize(f, 64*1024)
	// Write failures degrade to a no-op sink (one WARN), matching the
	// open-failure path above: a naozhi-side disk error must not abort a
	// healthy run — propagating it would classify the run failed-transport
	// and Stop a microVM whose stream is fine (review PR-2b F8). Per-line
	// Flush keeps §6.1 crash durability: an abnormal process exit loses at
	// most the line being written, not a 64KB buffered tail.
	degraded := false
	sink = func(line []byte) error {
		if degraded {
			return nil
		}
		// R20260613-ARCH-2 / #2083: the reader (SandboxRunEvents) caps a
		// single NDJSON token at sandboxEventsMaxLineSize via bufio.Scanner.
		// The SSE decoder shares that exact ceiling
		// (agentcore.MaxEnvelopeLineBytes), so a line the decoder accepted is
		// normally readable back. This guard is the last line of defence
		// against any line that still reaches the cap (e.g. JSON re-encoding
		// of an at-ceiling envelope): writing it would make the reader's
		// scanner hit ErrTooLong and discard every subsequent line in the
		// file — turning one oversized frame into a silent loss of ALL later
		// events. Degrade gracefully instead: drop the oversized line with a
		// WARN and keep writing subsequent lines.
		// `line` here is the raw envelope without the trailing '\n' this sink
		// appends. The `>=` (not `>`) keeps the written form (line + '\n')
		// from exceeding the scanner's token max: when len(line) == cap we
		// drop, so every written line is < cap and line+'\n' <= cap.
		if len(line) >= sandboxEventsMaxLineSize {
			lg.Warn("cron sandbox: oversized event line dropped; will not be readable by scanner",
				"len", len(line))
			return nil
		}
		_, werr := w.Write(line)
		if werr == nil {
			werr = w.WriteByte('\n')
		}
		if werr == nil {
			werr = w.Flush()
		}
		if werr != nil {
			degraded = true
			lg.Warn("cron sandbox: event log write failed; further events not persisted", "err", werr)
		}
		return nil
	}
	closer = func() {
		if err := w.Flush(); err != nil && !degraded {
			lg.Warn("cron sandbox: event log flush failed", "err", err)
		}
		if err := f.Close(); err != nil {
			lg.Warn("cron sandbox: event log close failed", "err", err)
		}
	}
	return sink, closer
}

// SandboxRunEvents reads the persisted event log for one sandbox run
// (sandboxevents/<jobID>/<runID>.ndjson, §6.1 streaming-to-disk) and returns
// up to maxLines raw NDJSON lines (each one decoded-and-re-encoded JSON, no
// trailing newline). The dashboard run-detail view (RFC §7.3) renders these
// as the event stream — identical to a local session's message render.
//
// Returns (nil, nil) when the file does not exist (a local run, an
// events-disabled deploy, or a run whose sink degraded on open) so the
// caller renders an empty stream rather than an error. jobID/runID are
// shape-validated by the caller (dashboard handler) before reaching here;
// re-validated defensively to keep the path traversal-safe even on a
// future internal caller.
//
// maxLines caps the response: a 60-minute run can emit tens of thousands of
// frames, and the dashboard only needs a bounded tail-or-head. We keep the
// FIRST maxLines (the run's opening — boot + early turns are the most useful
// for "what happened / where did it break"); a truncated marker is appended
// so the UI can show "… N more events".
func (s *Scheduler) SandboxRunEvents(jobID, runID string, maxLines int) ([][]byte, bool, error) {
	if s == nil || s.storePath == "" {
		return nil, false, nil
	}
	if !IsValidID(jobID) || !IsValidID(runID) {
		return nil, false, fmt.Errorf("cron sandbox: invalid jobID/runID")
	}
	if maxLines <= 0 {
		maxLines = sandboxEventsDefaultMax
	}
	// Bound concurrent reads (mirrors transcriptSem): a non-blocking acquire
	// fails fast with ErrSandboxEventsBusy rather than letting a burst pin
	// unbounded scanner buffers [R20260613-SEC-5 / #2066].
	select {
	case sandboxEventsSem <- struct{}{}:
		defer func() { <-sandboxEventsSem }()
	default:
		return nil, false, ErrSandboxEventsBusy
	}
	path := filepath.Join(filepath.Dir(s.storePath), "sandboxevents", jobID, runID+".ndjson")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil // no event log — render empty stream
		}
		return nil, false, fmt.Errorf("cron sandbox: open event log: %w", err)
	}
	defer f.Close()

	out := make([][]byte, 0, maxLines)
	sc := bufio.NewScanner(f)
	// Cap a single stream-json line at sandboxEventsMaxLineSize (~1 MB):
	// large enough for a realistic tool-result frame, small enough that a
	// concurrent burst cannot pin gigabytes of scanner buffers.
	sc.Buffer(make([]byte, 64*1024), sandboxEventsMaxLineSize)
	truncated := false
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			continue // skip any partial/corrupt tail line
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		out = append(out, cp)
		// Check the cap AFTER appending: a file with exactly maxLines valid
		// lines must NOT report truncated. We only set the flag once we have
		// actually accumulated maxLines AND a further valid line exists, so
		// peek for the next valid line before declaring truncation.
		if len(out) >= maxLines {
			if hasMoreValidJSON(sc) {
				truncated = true
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		// Return what we have plus the error; the caller logs + still renders
		// the partial stream (a corrupt tail must not hide a healthy head).
		// A read error mid-stream means the tail is missing → truncated, so
		// the UI signals an incomplete stream rather than rendering it whole.
		return out, true, fmt.Errorf("cron sandbox: scan event log: %w", err)
	}
	return out, truncated, nil
}

// hasMoreValidJSON advances the scanner looking for one more valid-JSON line
// after the cap was hit, so truncated reflects "real events were dropped"
// rather than "the file ended exactly at the cap". Trailing blank/corrupt
// lines do not count as a dropped event. The scanner is already consumed by
// the caller's break, so advancing it here is safe.
func hasMoreValidJSON(sc *bufio.Scanner) bool {
	for sc.Scan() {
		if json.Valid(sc.Bytes()) {
			return true
		}
	}
	return false
}

// sandboxEventsDefaultMax bounds SandboxRunEvents when the caller passes a
// non-positive cap. 2000 frames covers a typical run's opening comfortably
// while keeping the response well under a megabyte for the dashboard.
const sandboxEventsDefaultMax = 2000

// sandboxEventsMaxLineSize caps a single NDJSON line on the sandbox event
// wire. It aliases agentcore.MaxEnvelopeLineBytes — the single source of
// truth shared with the SSE decoder (holdStream) — so the writer's accept
// ceiling and this reader's scanner token limit can never drift.
//
// R20260613-214326-ARCH-1 (#2083): a previous split (16MB writer / 1MB
// reader, R20260613-SEC-5 / #2066) let 1–16MB tool-result lines write but
// never read — the scanner hit bufio.ErrTooLong and silently dropped that
// line plus every later event. Reader-side memory is bounded by
// sandboxEventsSemCap (concurrent-read semaphore), not by shrinking this cap
// below the writer's.
const sandboxEventsMaxLineSize = agentcore.MaxEnvelopeLineBytes

// sandboxEventsSemCap bounds concurrent SandboxRunEvents reads, mirroring the
// dashboard transcript endpoint's transcriptSem (cap 8). Each in-flight read
// holds up to maxLines×64KB output plus a scanner buffer; without this gate a
// single authenticated client could fan out enough concurrent reads to exhaust
// memory [R20260613-SEC-5 / #2066]. A non-blocking acquire fails fast rather
// than parking goroutines.
const sandboxEventsSemCap = 8

// sandboxEventsSem limits concurrent SandboxRunEvents reads process-wide.
// Package-level (not per-Scheduler) because the bound protects the host's
// memory, of which there is one regardless of how many schedulers exist in a
// test binary.
var sandboxEventsSem = make(chan struct{}, sandboxEventsSemCap)

// deleteJobSandboxEvents removes a deleted job's sandboxevents subtree
// (sandboxevents/<jobID>/). Best-effort: a missing tree is fine. A 60-minute
// sandbox run can emit several MB; leaving this tree orphaned on job deletion is
// a bounded but observable disk leak. Called from deleteJobRuns after the runs/
// and runsnapshots/ subtrees are removed. R20260614-LOGIC-2.
func (s *Scheduler) deleteJobSandboxEvents(jobID string) {
	if s.storePath == "" || !IsValidID(jobID) {
		return
	}
	dir := filepath.Join(filepath.Dir(s.storePath), "sandboxevents", jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron sandbox: event subtree delete failed", "job_id", jobID, "err", err)
	}
}

// ErrSandboxEventsBusy is returned by SandboxRunEvents when the concurrency
// semaphore is saturated. The dashboard handler maps it to HTTP 503 so a
// burst fails fast instead of allocating unbounded scanner buffers.
var ErrSandboxEventsBusy = errors.New("cron sandbox: event reads busy")
