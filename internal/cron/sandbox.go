package cron

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/metrics"
)

// SandboxJob is the run-once unit handed to the sandbox placement
// (agentcore-cloud-sandbox RFC §3.1). One job = one microVM = one prompt;
// no resume, no reattach.
type SandboxJob struct {
	JobID  string
	RunID  string
	Prompt string
	// Model pins the CLI model inside the microVM ("" = image default).
	Model string
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
}

// executeSandbox runs one cron job at the sandbox placement and routes the
// outcome through the same finishRun terminal protocol as local runs. It
// owns no session-router state: no GetOrCreate, no Reset, no stubs — the
// microVM burns on completion, which is the whole point (RFC §3.3:
// structural elimination of the cron session leak).
//
// Known Phase 1 gaps (deliberate, next PR — RFC §6.5):
//   - no pending-record write before RunJob and no startup reconcile, so a
//     naozhi restart mid-run orphans the held stream: the microVM keeps
//     running (bounded by maxLifetime ≤ 60min) and the run record never
//     gets a terminal frame. TODO(agentcore §6.5): pending file + reconcile.
//   - DeleteJobByID during an in-flight sandbox run does not Stop the
//     microVM; the run completes (or hits maxLifetime) and finishRun
//     no-ops via recordTerminalResult's jobs[id] re-check. Cloud-side cost
//     of one orphan run is bounded; wiring delete→Stop needs the pending
//     record above to know the runtime session id.
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
			"sandbox placement does not support work_dir (Phase 1; use placement=local)")
		return
	}
	if s.sandbox == nil {
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxUnavailable, "",
			"sandbox placement not configured (cron.sandbox in config)")
		return
	}

	a.inflight.setPhase(PhaseSending)

	budget := s.execTimeout
	if budget <= 0 || budget > sandboxMaxRunDuration {
		budget = sandboxMaxRunDuration
	}
	ctx, cancel := context.WithTimeout(s.stopCtx, budget)
	defer cancel()

	sink, closeSink := s.sandboxEventSink(a.snap.jobID, a.runID, a.lg)
	outcome, err := s.sandbox.RunJob(ctx, SandboxJob{
		JobID:  a.snap.jobID,
		RunID:  a.runID,
		Prompt: a.prompt,
		Model:  a.model,
	}, sink)
	// Close (flush) the event log BEFORE any finishRun below broadcasts the
	// terminal frame — a dashboard client reacting to RunEnded must find the
	// complete log on disk, not race a buffered tail (review PR-2b F1).
	closeSink()
	if err != nil {
		// Pre-flight failure: the job never reached the platform (invalid
		// payload — e.g. empty prompt). Permanent, not transport.
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, "",
			"sandbox preflight: "+sanitiseRunErrMsg(err.Error()))
		return
	}

	switch outcome.State {
	case SandboxStateSuccess:
		s.finishSandboxRun(a, RunStateSucceeded, ErrClassNone, outcome.ResultText, "")
	case SandboxStateFailedClean:
		s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxFailed, outcome.ResultText,
			sanitiseRunErrMsg(outcome.ErrMsg))
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
			msg += " (microVM termination confirmed)"
		} else {
			msg += " (microVM fate UNKNOWN — termination unconfirmed; check for side effects before re-running)"
		}
		state := RunStateFailed
		if ctx.Err() == context.DeadlineExceeded {
			state = RunStateTimedOut
		}
		s.finishSandboxRun(a, state, ErrClassSandboxTransport, outcome.ResultText, msg)
	}
}

// finishSandboxRun funnels every sandbox terminal path through finishRun
// (same three-write protocol as local runs: persist → metrics → broadcast)
// plus the completion notice.
func (s *Scheduler) finishSandboxRun(a sandboxExecArgs, state RunState, errClass ErrorClass, result, errMsg string) {
	if state == RunStateSucceeded {
		s.observeSuccessLatency(s.now().Sub(a.startedAt), SendResult{Text: result}, a.snap, a.lg)
	} else {
		metrics.CronSandboxRunFailedTotal.Add(1)
		a.lg.Error("cron sandbox run failed",
			"state", string(state), "err_class", string(errClass), "err", errMsg)
	}
	s.finishRun(finishArgs{
		job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
		state: state, errClass: errClass, errMsg: errMsg, result: result,
		prompt: a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
		finalizer: a.finalizer,
	})
	notice := "执行失败，请稍后重试。"
	if state == RunStateSucceeded {
		// Same pipeline as the local success path (R234-SEC-1 +
		// R20260531070014-ARCH-1): sanitise (truncate/redact) then localize
		// API-error envelopes before anything reaches an IM channel.
		notice = apierr.Localize(sanitiseRunResult(result))
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
