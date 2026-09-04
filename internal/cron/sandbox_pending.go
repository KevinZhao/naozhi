package cron

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// sandboxPending is the in-flight record written to
// <store-dir>/sandboxpending/<runID>.json before InvokeAgentRuntime and removed
// after the run reaches a terminal state. It exists to survive a naozhi
// restart: the held stream dies with the process but the microVM keeps
// running, and this file is the only handle the next boot has to Stop it.
type sandboxPending struct {
	JobID            string `json:"job_id"`
	RunID            string `json:"run_id"`
	RuntimeSessionID string `json:"runtime_session_id"`
	StartedAtMS      int64  `json:"started_at_ms"`
}

// sandboxPendingDir resolves the pending directory ("" when persistence is
// disabled — store-less test fixtures skip the §6.5 machinery entirely).
func (s *Scheduler) sandboxPendingDir() string {
	return s.stateSubtree("sandboxpending")
}

// writeSandboxPending persists the in-flight record. Returns the file path
// for the paired remove, or "" when persistence is off / the write failed
// (best-effort: §6.5 protection degrades to the maxLifetime bound, the run
// itself proceeds).
func (s *Scheduler) writeSandboxPending(p sandboxPending, lg *slog.Logger) string {
	dir := s.sandboxPendingDir()
	if dir == "" {
		return ""
	}
	// Symlink-guarded dir create: a planted `<stateDir>/sandboxpending → /elsewhere`
	// must not redirect the restart-reconcile handle (#2166). Degrades to no handle.
	if err := s.mkdirStateSubtree(dir); err != nil {
		lg.Warn("cron sandbox: pending dir create failed; restart reconcile unavailable for this run", "err", err)
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		lg.Warn("cron sandbox: pending marshal failed", "err", err)
		return ""
	}
	// runID is scheduler-generated hex — path-safe by construction.
	path := filepath.Join(dir, p.RunID+".json")
	// Atomic write: this file is the ONLY restart-reconcile handle; a truncated
	// record from a crash mid-write would be dropped as corrupt → permanent orphan.
	if err := osutil.WriteFileAtomic(path, b, 0o600); err != nil {
		lg.Warn("cron sandbox: pending write failed; restart reconcile unavailable for this run", "err", err)
		return ""
	}
	// Live jobID→path index lets DeleteJobByID find this run's pending file with
	// one lookup instead of scanning every record. The per-job CAS keeps at most
	// one in-flight run per job, so one entry per key is correct (#2140).
	s.setSandboxPendingIndex(p.JobID, path)
	return path
}

// setSandboxPendingIndex records jobID→path for the in-flight record. The map
// is allocated in NewScheduler, the only construction path reaching this seam.
func (s *Scheduler) setSandboxPendingIndex(jobID, path string) {
	s.sandboxPendingMu.Lock()
	s.sandboxPendingIndex[jobID] = path
	s.sandboxPendingMu.Unlock()
}

// clearSandboxPendingIndex drops the index entry for jobID iff it still maps
// to path (an unconditional delete could clobber a newer run's entry that
// reused the same jobID after a fast finish→re-run; the path guard makes the
// clear idempotent and race-safe against that re-write).
func (s *Scheduler) clearSandboxPendingIndex(jobID, path string) {
	if jobID == "" || path == "" {
		return
	}
	s.sandboxPendingMu.Lock()
	if s.sandboxPendingIndex[jobID] == path {
		delete(s.sandboxPendingIndex, jobID)
	}
	s.sandboxPendingMu.Unlock()
}

// lookupSandboxPendingIndex returns the recorded pending-file path for jobID
// (write-authoritative; "" when no in-flight record exists this process).
func (s *Scheduler) lookupSandboxPendingIndex(jobID string) string {
	s.sandboxPendingMu.RLock()
	path := s.sandboxPendingIndex[jobID]
	s.sandboxPendingMu.RUnlock()
	return path
}

// removeSandboxPending deletes the in-flight record after terminal state.
// "" path (write skipped/failed) is a no-op.
func removeSandboxPending(path string, lg *slog.Logger) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		lg.Warn("cron sandbox: pending remove failed; next start will reconcile a finished run (harmless Stop)", "err", err)
	}
}

// reconcileSandboxPending is the startup pass: every pending file is an
// orphaned run whose previous process died holding the stream. For each: Stop
// the microVM (idempotent), close the run record as failed-transport, drop the
// file. Runs asynchronously from Start() — Stops are network I/O. The terminal
// record goes through finishRun with a synthetic started event first so
// subscribers see a consistent started→ended pair.
//
// The validate/drop-corrupt pass is serial (local I/O); surviving orphans'
// Stops fan out across sandboxReconcileWorkers since each is an independent
// ~30s network call (#2142). reconcileOneSandboxOrphan is concurrency-safe.
func (s *Scheduler) reconcileSandboxPending() {
	dir := s.sandboxPendingDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cron sandbox: pending scan failed", "err", err)
		}
		return
	}

	type orphan struct {
		p    sandboxPending
		path string
	}
	orphans := make([]orphan, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Bail on shutdown so N×30s Stop timeouts don't exhaust gcWaitBudget.
		if s.stopCtx.Err() != nil {
			return
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("cron sandbox: pending read failed; skipping", "file", osutil.SanitizeForLog(e.Name(), 256), "err", err)
			continue
		}
		var p sandboxPending
		if err := json.Unmarshal(raw, &p); err != nil || !IsValidID(p.RunID) || !IsValidID(p.JobID) || p.StartedAtMS <= 0 || p.RuntimeSessionID == "" {
			// Corrupt or tampered record: RunID/JobID flow into run-record paths and the
			// broadcast, StartedAtMS<=0 would produce a 1970 StartedAt and an astronomical
			// DurationMS, and a record without a RuntimeSessionID cannot be reconciled
			// (Stop would be skipped yet finishRun + remove would still run). Drop it so
			// it does not re-warn on every boot.
			slog.Warn("cron sandbox: corrupt pending record dropped", "file", osutil.SanitizeForLog(e.Name(), 256), "err", err)
			_ = os.Remove(path)
			continue
		}
		orphans = append(orphans, orphan{p: p, path: path})
	}

	if len(orphans) == 0 {
		return
	}
	// Single orphan: skip the goroutine + channel plumbing.
	if len(orphans) == 1 {
		// Honor shutdown before the (up to sandboxStopTimeout) Stop, mirroring the
		// parallel path.
		if s.stopCtx.Err() != nil {
			return
		}
		s.reconcileOneSandboxOrphan(orphans[0].p, orphans[0].path)
		return
	}

	workers := sandboxReconcileWorkers
	if workers > len(orphans) {
		workers = len(orphans)
	}
	jobs := make(chan orphan)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for o := range jobs {
				// Per-orphan shutdown bail: stop dispatching new Stops so gcWaitBudget is not
				// exhausted; an in-flight Stop unblocks via its WithTimeout(stopCtx, …).
				if s.stopCtx.Err() != nil {
					continue
				}
				s.reconcileOneSandboxOrphan(o.p, o.path)
			}
		}()
	}
	// Select on stopCtx while feeding the unbuffered channel: workers stop
	// dispatching on shutdown but keep draining, so a plain send would park until
	// every orphan was handed off and hold close(jobs)/wg.Wait() hostage.
	for _, o := range orphans {
		select {
		case jobs <- o:
		case <-s.stopCtx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

// orphanDecision is what reconcile owes a single orphan once the Stop and the
// terminal-record probe have been observed. Output of the pure classifier
// classifyOrphanPending; reconcileOneSandboxOrphan only executes it (#2172).
type orphanDecision uint8

const (
	// orphanKeepPending: the microVM's fate is unknown or cannot be confirmed
	// from this process — leave the pending file so the NEXT start retries.
	orphanKeepPending orphanDecision = iota
	// orphanRemoveOnly: the Stop confirmed but the run is ALREADY terminal on
	// disk (an in-process transport failure drove finishRun before the process
	// died, #2054). A second finish would double-count counters + metrics.
	orphanRemoveOnly
	// orphanRemoveAfterFinish: Stop confirmed and no terminal record exists
	// (or the record is unparseable and can never be confirmed terminal) —
	// close the run as failed-transport, then drop the file.
	orphanRemoveAfterFinish
)

// orphanReason explains an orphanDecision so the orchestrator can log the
// exact line operators already grep for. Only Keep/RemoveOnly carry one.
type orphanReason uint8

const (
	orphanReasonNone orphanReason = iota
	// orphanReasonSandboxUnconfigured: cron.sandbox removed between restarts —
	// no Stop primitive this boot.
	orphanReasonSandboxUnconfigured
	// orphanReasonInvalidSessionID: RuntimeSessionID read from an operator-
	// writable file fails the production-format check (or is empty); without a
	// handle containment cannot be satisfied, so keeping the file is the only move.
	orphanReasonInvalidSessionID
	// orphanReasonStopFailed: StopSession returned an error — fate unknown.
	orphanReasonStopFailed
	// orphanReasonAlreadyTerminal: runs/<job>/<run>.json has EndedAt set.
	orphanReasonAlreadyTerminal
	// orphanReasonProbeTransient: s.Run failed with something other than
	// fs.ErrNotExist / ErrCorruptRun (EIO / ESTALE / EACCES …, #2149) — the record
	// may be terminal, so finishing now could double-count.
	orphanReasonProbeTransient
)

// orphanVerdict pairs the decision with its reason.
type orphanVerdict struct {
	decision orphanDecision
	reason   orphanReason
}

// orphanProbe is every fact classifyOrphanPending consumes: two static facts
// from the pending record + the outcome of the two I/O steps, gathered in
// order by probeOrphan. Fields for steps that were never attempted stay at
// their zero value; the classifier's rule order guarantees they are never
// consulted in that case (an earlier rule already decided Keep).
type orphanProbe struct {
	sandboxConfigured bool
	runtimeSessionID  string
	// stopErr is StopSession's result. Only meaningful when orphanStopBlocked
	// reports false for the two static facts above.
	stopErr error
	// rec / recErr are s.Run(jobID, runID)'s result. Only meaningful when
	// stopErr == nil.
	rec    *CronRun
	recErr error
}

// orphanStopBlocked reports whether the Stop must NOT be attempted for this
// record and why. Shared by probeOrphan (to skip the I/O) and
// classifyOrphanPending (rules 1–2) so the two cannot drift.
func orphanStopBlocked(sandboxConfigured bool, runtimeSessionID string) (orphanReason, bool) {
	if !sandboxConfigured {
		return orphanReasonSandboxUnconfigured, true
	}
	if !isValidRuntimeSessionID(runtimeSessionID) {
		return orphanReasonInvalidSessionID, true
	}
	return orphanReasonNone, false
}

// classifyOrphanPending is the pure containment state machine for one orphan;
// rules in order (first match wins). fs.ErrNotExist and ErrCorruptRun
// deliberately fall through rule 5 to rule 6.
//
//  1. sandbox not configured            → Keep   (retry handle must survive)
//  2. RuntimeSessionID invalid or empty → Keep   (cannot confirm fate)
//  3. Stop returned an error            → Keep   (containment unsatisfied)
//  4. run record present and EndedAt≠0 → RemoveOnly (#2054: already finished)
//  5. record probe failed transiently   → Keep   (#2149: may be terminal)
//  6. otherwise                         → RemoveAfterFinish
func classifyOrphanPending(pr orphanProbe) orphanVerdict {
	if reason, blocked := orphanStopBlocked(pr.sandboxConfigured, pr.runtimeSessionID); blocked {
		return orphanVerdict{orphanKeepPending, reason}
	}
	if pr.stopErr != nil {
		return orphanVerdict{orphanKeepPending, orphanReasonStopFailed}
	}
	if pr.recErr == nil && pr.rec != nil && !pr.rec.EndedAt.IsZero() {
		return orphanVerdict{orphanRemoveOnly, orphanReasonAlreadyTerminal}
	}
	if pr.recErr != nil && !errors.Is(pr.recErr, fs.ErrNotExist) && !errors.Is(pr.recErr, ErrCorruptRun) {
		return orphanVerdict{orphanKeepPending, orphanReasonProbeTransient}
	}
	return orphanVerdict{orphanRemoveAfterFinish, orphanReasonNone}
}

// probeOrphan gathers the orphanProbe for classifyOrphanPending: Stop the
// microVM (unless orphanStopBlocked), then — only after a confirmed Stop —
// probe the durable run record. The ordering is load-bearing: the record
// probe is meaningless while the microVM's fate is unknown, and a failed
// Stop must short-circuit before any local read.
func (s *Scheduler) probeOrphan(p sandboxPending) orphanProbe {
	pr := orphanProbe{sandboxConfigured: s.sandbox != nil, runtimeSessionID: p.RuntimeSessionID}
	if _, blocked := orphanStopBlocked(pr.sandboxConfigured, pr.runtimeSessionID); blocked {
		return pr
	}
	ctx, cancel := context.WithTimeout(s.stopCtx, sandboxStopTimeout)
	pr.stopErr = s.sandbox.StopSession(ctx, p.RuntimeSessionID)
	cancel()
	if pr.stopErr != nil {
		return pr
	}
	pr.rec, pr.recErr = s.Run(p.JobID, p.RunID)
	return pr
}

// logOrphanVerdict emits the operator-facing line for a Keep / RemoveOnly
// verdict at the log level matching that reason. RemoveAfterFinish has no
// line of its own — finishRun's own logging covers it.
func logOrphanVerdict(lg *slog.Logger, v orphanVerdict, pr orphanProbe) {
	switch v.reason {
	case orphanReasonSandboxUnconfigured:
		lg.Warn("cron sandbox: orphaned run found but sandbox not configured; keeping pending record until config returns")
	case orphanReasonInvalidSessionID:
		lg.Warn("cron sandbox: orphan pending record has invalid RuntimeSessionID format; keeping for manual inspection",
			"runtime_session_id", pr.runtimeSessionID)
	case orphanReasonStopFailed:
		lg.Error("cron sandbox: orphan Stop failed; keeping pending record for next start", "err", pr.stopErr)
	case orphanReasonAlreadyTerminal:
		lg.Info("cron sandbox: orphan already finished in-process; skipping duplicate finish",
			"state", pr.rec.State)
	case orphanReasonProbeTransient:
		lg.Warn("cron sandbox: orphan terminal-state probe failed transiently; keeping pending record for next start", "err", pr.recErr)
	}
}

// reconcileOneSandboxOrphan handles a single §6.5 orphan: Stop, terminal
// record, file removal. It is orchestration only — probeOrphan performs the
// I/O, classifyOrphanPending decides, finishOrphanRun closes the record.
// Stop failure keeps the file so the NEXT start retries — until a Stop is
// confirmed the microVM's fate is unknown and §6.2 containment is not
// satisfied.
func (s *Scheduler) reconcileOneSandboxOrphan(p sandboxPending, path string) {
	lg := slog.With("job_id", p.JobID, "run_id", p.RunID)
	lg.Warn("cron sandbox: reconciling orphaned run from previous process")

	pr := s.probeOrphan(p)
	v := classifyOrphanPending(pr)
	logOrphanVerdict(lg, v, pr)
	switch v.decision {
	case orphanKeepPending:
		return
	case orphanRemoveOnly:
		removeReconciledPending(path, lg)
		return
	}

	// orphanRemoveAfterFinish. The job may have been deleted while we were
	// down — finishRun's recordTerminalResult re-checks s.jobs[id] and no-ops
	// the persist; the broadcast pair still closes subscriber timelines.
	js, j := s.snapshotOrphanJob(p.JobID)
	// Re-check job existence under RLock ONCE before any subscriber-visible write:
	// a concurrent DeleteJobByID in the gap since the snapshot deletes the job and
	// sweeps the attention queue, so writing would leave a ghost attention card and
	// a phantom started/ended pair (#2156). The SAME boolean feeds both the
	// attention write and finishOrphanRun so they agree.
	jobExists := j != nil && s.jobExists(p.JobID)
	if jobExists {
		s.maybeEnqueueOrphanAttention(p, js, lg)
	}
	s.finishOrphanRun(p, js, j, jobExists, lg)
	removeReconciledPending(path, lg)
}

// orphanJobSnapshot is the subset of *Job the orphan finish needs, copied
// under s.mu.RLock (UpdateJob mutates *Job in place under s.mu.Lock, so any
// lock-free read is a data race).
type orphanJobSnapshot struct {
	sideEffects  bool
	label        string
	freshContext bool
	prompt       string
	workDir      string
}

// snapshotOrphanJob returns the lock-safe field snapshot plus the *Job
// pointer (nil when the job no longer exists). Passing j on to finishRun is
// safe: finishRun re-locks (recordTerminalResult re-checks s.jobs[id]) —
// only THIS file's lock-free reads need to be snapshots.
func (s *Scheduler) snapshotOrphanJob(jobID string) (orphanJobSnapshot, *Job) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j := s.jobs[jobID]
	if j == nil {
		return orphanJobSnapshot{}, nil
	}
	return orphanJobSnapshot{
		sideEffects:  j.SideEffects != nil && *j.SideEffects,
		label:        jobTitleOrFallback(j),
		freshContext: j.FreshContext,
		prompt:       j.Prompt,
		workDir:      j.WorkDir,
	}, j
}

// jobExists re-checks s.jobs[jobID] under RLock (the COR-001 TOCTOU guard).
func (s *Scheduler) jobExists(jobID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[jobID] != nil
}

// maybeEnqueueOrphanAttention enqueues a live side-effecting job's orphan for
// human confirmation: the microVM was Stopped, but it may have completed and
// produced its side effect before naozhi died — only a human can tell. A
// side-effect-free orphan stays a plain failed-transport record.
//
// An in-process transport failure may have ALREADY enqueued a record for this
// runID (reason=transport); an unconditional write would clobber it and
// downgrade the reason to "orphaned" (#2119). Probe first; a read error is
// treated as "may exist" → skip.
func (s *Scheduler) maybeEnqueueOrphanAttention(p sandboxPending, js orphanJobSnapshot, lg *slog.Logger) {
	if !js.sideEffects {
		return
	}
	rec, qok, qerr := s.getSandboxAttention(p.RunID)
	if qerr != nil {
		lg.Warn("cron sandbox: attention probe failed; keeping any existing record, skipping orphaned write", "err", qerr)
		return
	}
	if qok || rec != nil {
		return
	}
	s.writeSandboxAttention(sandboxAttention{
		JobID:            p.JobID,
		RunID:            p.RunID,
		RuntimeSessionID: p.RuntimeSessionID,
		Reason:           attentionReasonOrphaned,
		JobLabel:         js.label,
		StartedAtMS:      p.StartedAtMS,
		CreatedAtMS:      s.attentionNowMS(),
	}, lg)
}

// Every reconciled orphan closes with the same terminal classification; both
// finishOrphanRun branches read these so they cannot disagree on which
// per-state buckets advance.
const (
	orphanTerminalState    = RunStateFailed
	orphanTerminalErrClass = ErrClassSandboxTransport
	orphanTerminalErrMsg   = "naozhi restarted while the run was in flight; microVM terminated by startup reconcile"
)

// finishOrphanRun is the SINGLE convergence point for the orphan's terminal
// accounting (#2172). jobExists=true: full protocol — synthetic started frame +
// finishRun (persist → bumpRunStateMetrics → broadcast → CronRunEndedTotal).
// jobExists=false (job deleted while naozhi was down or in the snapshot→finish
// gap, #2156): metrics-only mirror in the SAME order — CronRunStartedTotal →
// bumpRunStateMetrics → CronRunEndedTotal — with the broadcast halves removed,
// since a started/ended pair for a job the dashboard already dropped would be a
// phantom lifecycle. TestReconcileOrphan_TerminalCounterParity pins that both
// branches move every counter identically.
func (s *Scheduler) finishOrphanRun(p sandboxPending, js orphanJobSnapshot, j *Job, jobExists bool, lg *slog.Logger) {
	startedAt := time.UnixMilli(p.StartedAtMS)
	if !jobExists {
		metrics.CronRunStartedTotal.Add(1)               // 1. emitRunStarted's bump
		s.bumpRunStateMetrics(orphanTerminalState, true) // 2. finishRun's per-state bump
		metrics.CronRunEndedTotal.Add(1)                 // 3. finishRun's final bump
		lg.Info("cron sandbox: orphan's job no longer exists; closing record file only")
		return
	}
	// Synthetic started so subscribers get a paired lifecycle (the real
	// started frame belonged to the previous process's broadcaster).
	s.emitRunStarted(RunStartedEvent{
		JobID:     p.JobID,
		RunID:     p.RunID,
		StartedAt: startedAt,
		Trigger:   runtelemetry.TriggerScheduled,
		Fresh:     js.freshContext,
	})
	// NIL inflight deliberately: the orphan belongs to the PREVIOUS process, so
	// this process's CAS gate was never taken for it. The same job's run-B may be
	// live RIGHT NOW holding the gate; a finalizer bound to s.jobInflight(jobID)
	// would Store(false) run-B's gate and let a third tick double-run.
	s.finishRun(finishArgs{
		job: j, runID: p.RunID, startedAt: startedAt,
		trigger: runtelemetry.TriggerScheduled,
		state:   orphanTerminalState, errClass: orphanTerminalErrClass,
		errMsg: orphanTerminalErrMsg,
		prompt: js.prompt, workDir: js.workDir, fresh: js.freshContext,
		finalizer: &runFinalizer{},
		sandbox:   true,
	})
}

// removeReconciledPending drops the pending file once reconcile has
// discharged everything it owed for the orphan.
func removeReconciledPending(path string, lg *slog.Logger) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		lg.Warn("cron sandbox: reconciled pending remove failed", "err", err)
	}
}

// stopSandboxRunsForJob terminates any in-flight sandbox microVM for a job
// being deleted, using the runtime session id in the pending record; otherwise
// the run would finish or hit maxLifetime, burning cost and possibly producing
// side effects the operator no longer wants. Runs lock-free from
// deleteJobPostCleanup. Best-effort and idempotent: the common case resolves
// the pending file via sandboxPendingIndex (#2140) and only falls back to a dir
// scan for files written by a previous process; StopSession is idempotent; the
// file is removed after a confirmed Stop and KEPT on failure (fate unknown).
// No terminal CronRun is written here: the run's own goroutine still reaches
// finishRun, which no-ops the persist for the deleted job.
func (s *Scheduler) stopSandboxRunsForJob(jobID string) {
	if s.sandbox == nil {
		return // sandbox placement not configured — nothing could be in flight
	}
	dir := s.sandboxPendingDir()
	if dir == "" {
		return
	}
	// Fast path: this process wrote the record, so its path is in the index.
	if path := s.lookupSandboxPendingIndex(jobID); path != "" {
		if s.stopOneSandboxPendingFile(jobID, path) {
			s.clearSandboxPendingIndex(jobID, path)
		}
		return
	}
	// Slow path (index miss): a pending file left by a previous process; scan for
	// a JobID match.
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cron sandbox: delete-stop pending scan failed", "job_id", jobID, "err", err)
		}
		return
	}
	for _, e := range entries {
		// Bail on shutdown so N×30s StopSession calls don't exhaust gcWaitBudget.
		if s.stopCtx.Err() != nil {
			return
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // benign: the run goroutine may have just removed it
		}
		var p sandboxPending
		if err := json.Unmarshal(raw, &p); err != nil || p.JobID != jobID {
			continue
		}
		if s.stopOneSandboxPendingFile(jobID, path) {
			s.clearSandboxPendingIndex(jobID, path)
		}
	}
}

// stopOneSandboxPendingFile reads, validates, and (on a valid record) Stops the
// microVM for a single §6.5 pending file, removing the file on a confirmed
// Stop. Returns true when the file was removed (so the caller can drop the
// index entry); false when the record was skipped (corrupt/invalid/unreadable)
// or the Stop was not confirmed — in which case the file is KEPT (§6.2) for the
// next startup reconcile.
func (s *Scheduler) stopOneSandboxPendingFile(jobID, path string) bool {
	if s.stopCtx.Err() != nil {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false // benign: the run goroutine may have just removed it
	}
	var p sandboxPending
	// RunID is validated too: it is read from operator-writable disk and flows
	// into slog fields, so a tampered file could inject control characters.
	if err := json.Unmarshal(raw, &p); err != nil || p.JobID != jobID || p.RuntimeSessionID == "" || !IsValidID(p.RunID) {
		return false
	}
	// Validate RuntimeSessionID from disk before StopSession; on invalid format
	// skip and keep the file for startup reconcile.
	if !isValidRuntimeSessionID(p.RuntimeSessionID) {
		slog.Warn("cron sandbox: delete-stop skipped — pending record has invalid RuntimeSessionID format",
			"job_id", jobID, "run_id", p.RunID, "runtime_session_id", p.RuntimeSessionID)
		return false
	}
	lg := slog.With("job_id", jobID, "run_id", p.RunID)
	lg.Info("cron sandbox: deleting job with in-flight run; stopping microVM")
	ctx, cancel := context.WithTimeout(s.stopCtx, sandboxStopTimeout)
	stopErr := s.sandbox.StopSession(ctx, p.RuntimeSessionID)
	cancel()
	if stopErr != nil {
		// Keep the file: §6.2 — fate unknown until a confirmed Stop.
		// Startup reconcile retries. The deletion itself still proceeds.
		lg.Error("cron sandbox: delete-stop failed; pending record kept for startup reconcile", "err", stopErr)
		return false
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		lg.Warn("cron sandbox: delete-stop pending remove failed", "err", err)
	}
	return true
}
