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
	"strings"
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

// isValidRuntimeSessionID reports whether s has the exact shape produced by
// sandboxRuntimeSessionID ("run-<hex-runID>-<unix-nanos>"). The three
// restart/replay paths (reconcileOneSandboxOrphan, stopSandboxRunsForJob,
// ReplaySandboxRun) read this field from operator-writable disk files and
// pass it straight to SandboxRunner.StopSession → AWS Bedrock. Every other
// disk-read identifier (RunID/JobID) is IsValidID-checked; this restores the
// same defense-in-depth so a hand-crafted pending/attention record cannot
// inject an over-long value or control/whitespace characters into the AWS
// SDK call (R20260613-SEC-2, #2065).
//
// Decomposed against the generator's structure rather than a fixed-width
// regexp so it tracks IsValidID's hex charset / 64-byte ceiling automatically
// if the runID schema is ever widened.
func isValidRuntimeSessionID(s string) bool {
	const prefix = "run-"
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return false
	}
	// Split into the hex runID and the decimal nano suffix at the LAST '-':
	// the runID charset (lowercase hex) never contains '-', so the final '-'
	// is unambiguously the runID/nanos boundary.
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 || i == len(rest)-1 {
		return false
	}
	runID, nanos := rest[:i], rest[i+1:]
	if !IsValidID(runID) {
		return false
	}
	for j := 0; j < len(nanos); j++ {
		if nanos[j] < '0' || nanos[j] > '9' {
			return false
		}
	}
	return true
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
		// §6.2 rule 3 + §7.4: a side-effecting job's transport failure must NOT
		// auto-replay — it enters the human confirmation queue (the operator
		// checks whether the side effect already landed before deciding to
		// confirm-done or replay). A side-effect-free job is safe to re-run
		// freely, so it never enters the queue (its failed-transport record
		// still warns in history). RuntimeSessionID is carried so the queue's
		// replay action can satisfy §6.2 rule 1 (Stop before replay). Skip the
		// queue entry when Stop was already confirmed in-process: the microVM is
		// dead, but the SIDE EFFECT may still have landed before the stream
		// broke, so a side-effecting job still needs a human look — keep it
		// queued regardless of StopConfirmed.
		if a.snap.sideEffects {
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
		// R20260613-CR-6 (#2059): align shutdown-cancel classification with the
		// local path (scheduler_run.go). sandbox ctx = WithTimeout(s.stopCtx,
		// budget), so scheduler Stop cancels s.stopCtx → ctx.Err()=Canceled.
		// Treat that as RunStateCanceled with skipPersist=true (keep history
		// clean — a graceful shutdown is not a transport failure) rather than
		// recording a failed-transport run. DeadlineExceeded stays TimedOut.
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			s.finishSandboxRunSkipPersist(a, RunStateCanceled, ErrClassCanceled, outcome.ResultText,
				"sandbox run canceled by shutdown", metaPtr)
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.finishSandboxRun(a, RunStateTimedOut, ErrClassSandboxTransport, outcome.ResultText, msg, metaPtr)
		default:
			s.finishSandboxRun(a, RunStateFailed, ErrClassSandboxTransport, outcome.ResultText, msg, metaPtr)
		}
	}
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
	} else {
		metrics.CronSandboxRunFailedTotal.Add(1)
		a.lg.Error("cron sandbox run failed",
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
	// Bound concurrent reads: each in-flight call parks a scanner buffer that
	// can grow toward sandboxEventsMaxToken plus up to maxLines retained
	// frames, so a single authenticated client issuing a burst of event-log
	// reads is a memory amplifier. Mirror the dashboard transcriptSem gate
	// (R20260613-SEC-5, #2066) with a non-blocking acquire: when the gate is
	// saturated return ErrSandboxEventsBusy so the handler fails fast (503)
	// instead of letting N requests pile up resident buffers.
	select {
	case sandboxEventsSem <- struct{}{}:
		defer func() { <-sandboxEventsSem }()
	default:
		return nil, false, ErrSandboxEventsBusy
	}
	if maxLines <= 0 {
		maxLines = sandboxEventsDefaultMax
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
	// Match the writer/agentcore ceiling: a single stream-json line can carry
	// a large tool result. bufio starts at the 64 KB initial buffer and only
	// grows toward the max when an actual line demands it, so the typical
	// read stays at 64 KB; the ceiling must stay >= the agentcore producer's
	// max (internal/agentcore/client.go) or a frame the system wrote could
	// not be read back.
	sc.Buffer(make([]byte, 64*1024), sandboxEventsMaxToken)
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

// sandboxEventsMaxToken is the bufio.Scanner max token size for one NDJSON
// frame. It must stay >= the agentcore producer ceiling
// (internal/agentcore/client.go: (16<<20)+(64<<10)) so any frame the system
// wrote to the event log can be read back; a lower cap would silently turn a
// large-but-valid frame into a skipped/corrupt line.
const sandboxEventsMaxToken = (16 << 20) + (64 << 10)

// sandboxEventsSemCap bounds concurrent SandboxRunEvents reads process-wide.
// Mirrors the dashboard transcriptSem cap (8): each in-flight read parks a
// scanner buffer (growing toward sandboxEventsMaxToken) plus its retained
// frames, so an unbounded fan-out under multi-operator load is a memory
// amplifier (R20260613-SEC-5, #2066).
const sandboxEventsSemCap = 8

// sandboxEventsSem is the process-wide concurrency gate for
// SandboxRunEvents. A non-blocking acquire keeps the failure mode "busy →
// ErrSandboxEventsBusy" (handler maps to 503) rather than queueing requests
// that each hold resident buffers.
var sandboxEventsSem = make(chan struct{}, sandboxEventsSemCap)

// ErrSandboxEventsBusy is returned by SandboxRunEvents when the concurrency
// gate (sandboxEventsSem) is saturated. The dashboard handler maps it to a
// 503 so the client retries instead of amplifying resident memory.
var ErrSandboxEventsBusy = errors.New("cron sandbox: run events read busy")
