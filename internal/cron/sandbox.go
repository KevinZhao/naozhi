package cron

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/naozhi/naozhi/internal/costledger"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/limits"
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

	budget := s.execTimeout
	if budget <= 0 || budget > sandboxMaxRunDuration {
		budget = sandboxMaxRunDuration
	}
	ctx, cancel := context.WithTimeout(s.stopCtx, budget)
	defer cancel()

	// Pending record persisted BEFORE the invoke so a restart mid-hold can Stop the
	// orphaned microVM and close the run. Best-effort: a write failure only loses
	// restart immunity (orphan bounded by maxLifetime), it does not fail the run.
	runtimeSID := sandboxRuntimeSessionID(a.runID, a.startedAt)
	pendingPath := s.writeSandboxPending(sandboxPending{
		JobID:            a.snap.jobID,
		RunID:            a.runID,
		RuntimeSessionID: runtimeSID,
		StartedAtMS:      a.startedAt.UnixMilli(),
	}, a.lg)

	// Input snapshot (content-addressed prompt + model) persisted BEFORE the invoke
	// so a replay re-injects the exact payload. No secrets are injected yet, and
	// the image version is unknown until the run reports it. Best-effort.
	s.writeSandboxSnapshot(a.snap.jobID, a.runID, a.prompt, a.model, "", nil, a.lg)

	sink, closeSink := s.sandboxEventSink(a.snap.jobID, a.runID, a.lg)
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
		removeSandboxPending(pendingPath, a.lg)
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
		removeSandboxPending(pendingPath, a.lg)
		s.clearSandboxPendingIndex(a.snap.jobID, pendingPath)
		s.finishSandboxRun(a, RunStateSucceeded, ErrClassNone, outcome.ResultText, "", metaPtr)
	case SandboxStateFailedClean:
		removeSandboxPending(pendingPath, a.lg)
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
			removeSandboxPending(pendingPath, a.lg)
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
	// s.jobs[id] (mirrors recordTerminalResult) and skip if the job is gone.
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
	s.finishRun(finishArgs{
		job: a.job, runID: a.runID, startedAt: a.startedAt, trigger: a.trigger,
		state: state, errClass: errClass, errMsg: errMsg, result: result,
		skipPersist: skipPersist,
		prompt:      a.snap.prompt, workDir: a.snap.workDir, fresh: a.snap.fresh,
		finalizer:   a.finalizer,
		sandboxMeta: meta,
		replayOf:    a.replayOf,
		sandbox:     true,
	})
	// A shutdown-cancel is not a user-visible failure — no notice, mirroring the
	// local path (#2059).
	if state == RunStateCanceled {
		return
	}
	notice := "执行失败，请稍后重试。"
	if state == RunStateSucceeded {
		// Same pipeline as the local success path: sanitise then localize API-error
		// envelopes before anything reaches IM.
		notice = localizeNotice(result)
	} else if errClass == ErrClassSandboxTransport {
		notice = "云沙箱连接中断，任务状态未知，请检查执行历史。"
	}
	s.deliverNotice(a.notifyTo, formatCronNotice(a.snap.labelOrID(), notice))
}

// sandboxEventSink opens the per-run event log
// (<store-dir>/sandboxevents/<jobID>/<runID>.ndjson) and returns a sink
// writing one envelope per line, plus a closer. Streaming to disk means the
// events received before a mid-job stream break are already durable. On open
// failure the sink degrades to a no-op with one WARN (the run is more valuable
// than its event log). Deliberately separate from the runStore's runs/ tree.
func (s *Scheduler) sandboxEventSink(jobID, runID string, lg *slog.Logger) (sink func([]byte) error, closer func()) {
	if s.storePath == "" {
		return func([]byte) error { return nil }, func() {}
	}
	dir := s.stateSubtree("sandboxevents", jobID)
	if err := s.mkdirStateSubtree(dir); err != nil {
		lg.Warn("cron sandbox: event log dir create failed; events not persisted", "err", err)
		return func([]byte) error { return nil }, func() {}
	}
	// runID is scheduler-generated hex, path-safe by construction; join
	// defensively anyway.
	f, err := os.OpenFile(filepath.Join(dir, runID+".ndjson"),
		os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		lg.Warn("cron sandbox: event log open failed; events not persisted", "err", err)
		return func([]byte) error { return nil }, func() {}
	}
	w := bufio.NewWriterSize(f, 64*1024)
	// Write failures degrade to a no-op sink (one WARN): a naozhi-side disk error
	// must not abort a healthy run — propagating it would classify the run
	// failed-transport and Stop a microVM whose stream is fine. Per-line Flush
	// keeps crash durability to at most the line being written.
	degraded := false
	sink = func(line []byte) error {
		if degraded {
			return nil
		}
		// The reader (SandboxRunEvents) caps a token at sandboxEventsMaxLineSize; a
		// line reaching it would make the scanner hit ErrTooLong and drop every later
		// event, so drop just the oversized line with a WARN instead (#2083). `>=`
		// keeps line+'\n' <= cap.
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
	// Single fd-release path, idempotent via sync.Once so callers can order the
	// explicit flush before the RunEnded broadcast AND `defer closeSink()` as a
	// panic-safe fallback without double-closing (#2317).
	var closeOnce sync.Once
	closer = func() {
		closeOnce.Do(func() {
			if err := w.Flush(); err != nil && !degraded {
				lg.Warn("cron sandbox: event log flush failed", "err", err)
			}
			if err := f.Close(); err != nil {
				lg.Warn("cron sandbox: event log close failed", "err", err)
			}
		})
	}
	return sink, closer
}

// SandboxRunEvents reads the persisted event log for one sandbox run
// (sandboxevents/<jobID>/<runID>.ndjson) and returns up to maxLines raw NDJSON
// lines (no trailing newline) for the dashboard run-detail event stream.
//
// Returns (nil, nil) when the file does not exist (local run, events disabled,
// sink degraded on open) so the caller renders an empty stream. jobID/runID
// are re-validated defensively for path safety. maxLines keeps the FIRST
// maxLines (boot + early turns are the most useful for "where did it break");
// the truncated flag lets the UI show "… N more events".
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
	// Bound concurrent reads: a non-blocking acquire fails fast with
	// ErrSandboxEventsBusy rather than letting a burst pin unbounded scanner
	// buffers (#2066).
	select {
	case sandboxEventsSem <- struct{}{}:
		defer func() { <-sandboxEventsSem }()
	default:
		return nil, false, ErrSandboxEventsBusy
	}
	path := s.stateSubtree("sandboxevents", jobID, runID+".ndjson")
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
	// Cap a single line at sandboxEventsMaxLineSize so a concurrent burst cannot
	// pin gigabytes of scanner buffers.
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
		// A file with exactly maxLines valid lines must NOT report truncated: peek
		// for a further valid line first.
		if len(out) >= maxLines {
			if hasMoreValidJSON(sc) {
				truncated = true
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		// Return the partial head plus the error; a missing tail means truncated, so
		// the UI signals an incomplete stream.
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
// wire. It must equal agentcore.MaxEnvelopeLineBytes (the SSE decoder's
// ceiling) so the writer's accept ceiling and this reader's scanner token limit
// never drift: a writer/reader split let lines write but never read back,
// silently dropping every later event (#2083). cron cannot import
// internal/agentcore (AWS SDK; no_agentcore_import_test.go pins the edge), so
// both derive from limits.MaxStreamJSONLine + 64KiB. Reader memory is bounded
// by sandboxEventsSemCap, not by shrinking this cap.
const sandboxEventsMaxLineSize = limits.MaxStreamJSONLine + (64 << 10)

// sandboxEventsSemCap bounds concurrent SandboxRunEvents reads (mirrors the
// dashboard transcriptSem). Each read holds up to maxLines×64KB plus a scanner
// buffer; without the gate one authenticated client could exhaust memory
// (#2066). Non-blocking acquire fails fast rather than parking goroutines.
const sandboxEventsSemCap = 8

// sandboxEventsSem limits concurrent SandboxRunEvents reads process-wide.
// Package-level (not per-Scheduler) because the bound protects the host's
// memory, of which there is one regardless of how many schedulers exist in a
// test binary.
var sandboxEventsSem = make(chan struct{}, sandboxEventsSemCap)

// deleteJobSandboxEvents removes a deleted job's sandboxevents subtree.
// Best-effort: a missing tree is fine. A 60-minute run can emit several MB, so
// leaving it orphaned would be an observable disk leak.
func (s *Scheduler) deleteJobSandboxEvents(jobID string) {
	if s.storePath == "" || !IsValidID(jobID) {
		return
	}
	dir := s.stateSubtree("sandboxevents", jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron sandbox: event subtree delete failed", "job_id", jobID, "err", err)
	}
}

// ErrSandboxEventsBusy is returned by SandboxRunEvents when the concurrency
// semaphore is saturated. The dashboard handler maps it to HTTP 503 so a
// burst fails fast instead of allocating unbounded scanner buffers.
var ErrSandboxEventsBusy = errors.New("cron sandbox: event reads busy")
