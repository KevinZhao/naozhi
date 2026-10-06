package cron

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// writeSandboxPending persists the in-flight record (sandboxstore) and indexes
// its path by job. Returns the path for the paired remove, or "" when
// persistence is off or the write failed (best-effort: §6.5 protection
// degrades to the maxLifetime bound, the run itself proceeds).
func (s *Scheduler) writeSandboxPending(p sandboxstore.Pending, lg *slog.Logger) string {
	path := s.sandboxState().WritePending(p, lg)
	if path == "" {
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
func (s *Scheduler) removeSandboxPending(path string, lg *slog.Logger) {
	if err := s.sandboxState().RemovePending(path); err != nil {
		lg.Warn("cron sandbox: pending remove failed; next start will reconcile a finished run (harmless Stop)", "err", err)
	}
}

// sandboxOrphan is one pending record claimed by claimSandboxOrphans.
type sandboxOrphan struct {
	p    sandboxstore.Pending
	path string
}

// reconcileSandboxPending is the whole startup pass in one call: every pending
// file is an orphaned run whose previous process died holding the stream. For
// each: Stop the microVM (idempotent), close the run record as failed-transport,
// drop the file. Start runs the two halves separately (see claimSandboxOrphans).
func (s *Scheduler) reconcileSandboxPending() {
	s.reconcileSandboxOrphans(s.claimSandboxOrphans())
}

// claimSandboxOrphans is the synchronous half, run by Start before the first
// tick: it lists the pending files before this process starts runs of its own,
// so a run started right after Start is never taken for an orphan and has its
// live microVM stopped. It validates each record and drops corrupt ones (local
// I/O only).
func (s *Scheduler) claimSandboxOrphans() []sandboxOrphan {
	entries, err := s.sandboxState().ListPending()
	if err != nil {
		slog.Warn("cron sandbox: pending scan failed", "err", err)
		return nil
	}

	orphans := make([]sandboxOrphan, 0, len(entries))
	for _, e := range entries {
		// Claim nothing once shutdown has begun; the files stay for the next start.
		if s.stopCtx.Err() != nil {
			return nil
		}
		if e.State == sandboxstore.PendingUnreadable {
			slog.Warn("cron sandbox: pending read failed; skipping", "file", osutil.SanitizeForLog(e.Name, 256))
			continue
		}
		p := e.Rec
		if e.State == sandboxstore.PendingCorrupt || !IsValidID(p.RunID) || !IsValidID(p.JobID) || p.StartedAtMS <= 0 || p.RuntimeSessionID == "" {
			// Corrupt or tampered record: RunID/JobID flow into run-record paths and the
			// broadcast, StartedAtMS<=0 would produce a 1970 StartedAt and an astronomical
			// DurationMS, and a record without a RuntimeSessionID cannot be reconciled
			// (Stop would be skipped yet finishRun + remove would still run). Drop it so
			// it does not re-warn on every boot.
			slog.Warn("cron sandbox: corrupt pending record dropped", "file", osutil.SanitizeForLog(e.Name, 256))
			_ = s.sandboxState().RemovePending(e.Path)
			continue
		}
		orphans = append(orphans, sandboxOrphan{p: p, path: e.Path})
	}
	return orphans
}

// reconcileSandboxOrphans is the asynchronous half: Stops are network I/O, so
// they fan out across sandboxReconcileWorkers, each an independent ~30s call
// (#2142). The terminal record goes through finishRun with a synthetic started
// event first so subscribers see a consistent started→ended pair.
// reconcileOneSandboxOrphan is concurrency-safe.
func (s *Scheduler) reconcileSandboxOrphans(orphans []sandboxOrphan) {
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
	jobs := make(chan sandboxOrphan)
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
func (s *Scheduler) probeOrphan(p sandboxstore.Pending) orphanProbe {
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
func (s *Scheduler) reconcileOneSandboxOrphan(p sandboxstore.Pending, path string) {
	lg := slog.With("job_id", p.JobID, "run_id", p.RunID)
	lg.Warn("cron sandbox: reconciling orphaned run from previous process")

	pr := s.probeOrphan(p)
	v := classifyOrphanPending(pr)
	logOrphanVerdict(lg, v, pr)
	switch v.decision {
	case orphanKeepPending:
		return
	case orphanRemoveOnly:
		s.removeReconciledPending(path, lg)
		return
	}

	// orphanRemoveAfterFinish. The job may have been deleted while we were
	// down — finishRun's recordTerminalResult re-checks s.tbl.jobs[id] and no-ops
	// the persist; the broadcast pair still closes subscriber timelines.
	js, found := s.snapshotOrphanJob(p.JobID)
	// Re-check job existence under RLock ONCE before any subscriber-visible write:
	// a concurrent DeleteJobByID in the gap since the snapshot deletes the job and
	// sweeps the attention queue, so writing would leave a ghost attention card and
	// a phantom started/ended pair (#2156). The SAME boolean feeds both the
	// attention write and finishOrphanRun so they agree.
	jobExists := found && s.jobExists(p.JobID)
	if jobExists {
		s.maybeEnqueueOrphanAttention(p, js, lg)
	}
	s.finishOrphanRun(p, js, jobExists, lg)
	s.removeReconciledPending(path, lg)
}

// orphanJobSnapshot is the subset of *Job the orphan finish needs, copied
// under s.tbl.mu.RLock (UpdateJob mutates *Job in place under s.tbl.mu.Lock, so any
// lock-free read is a data race).
type orphanJobSnapshot struct {
	sideEffects  bool
	label        string
	freshContext bool
	prompt       string
	workDir      string
}

// snapshotOrphanJob returns the fields an orphaned run's finish needs; ok is
// false when the job no longer exists.
func (s *Scheduler) snapshotOrphanJob(jobID string) (orphanJobSnapshot, bool) {
	snap, ok := s.tbl.runSnapshot(jobID)
	if !ok {
		return orphanJobSnapshot{}, false
	}
	return orphanJobSnapshot{
		sideEffects:  snap.sideEffects,
		label:        snap.label,
		freshContext: snap.fresh,
		prompt:       snap.prompt,
		workDir:      snap.workDir,
	}, true
}

// jobExists re-checks that jobID is registered (the COR-001 TOCTOU guard).
func (s *Scheduler) jobExists(jobID string) bool { return s.tbl.exists(jobID) }

// maybeEnqueueOrphanAttention enqueues a live side-effecting job's orphan for
// human confirmation: the microVM was Stopped, but it may have completed and
// produced its side effect before naozhi died — only a human can tell. A
// side-effect-free orphan stays a plain failed-transport record.
//
// An in-process transport failure may have ALREADY enqueued a record for this
// runID (reason=transport); an unconditional write would clobber it and
// downgrade the reason to "orphaned" (#2119). Probe first; a read error is
// treated as "may exist" → skip.
func (s *Scheduler) maybeEnqueueOrphanAttention(p sandboxstore.Pending, js orphanJobSnapshot, lg *slog.Logger) {
	if !js.sideEffects {
		return
	}
	rec, qok, qerr := s.sandboxState().GetAttention(p.RunID)
	if qerr != nil {
		lg.Warn("cron sandbox: attention probe failed; keeping any existing record, skipping orphaned write", "err", qerr)
		return
	}
	if qok || rec != nil {
		return
	}
	s.sandboxState().WriteAttention(sandboxstore.Attention{
		JobID:            p.JobID,
		RunID:            p.RunID,
		RuntimeSessionID: p.RuntimeSessionID,
		Reason:           sandboxstore.ReasonOrphaned,
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
func (s *Scheduler) finishOrphanRun(p sandboxstore.Pending, js orphanJobSnapshot, jobExists bool, lg *slog.Logger) {
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
	rc := runCtx{
		jobID: p.JobID, runID: p.RunID, startedAt: startedAt,
		trigger:   runtelemetry.TriggerScheduled,
		finalizer: &runFinalizer{},
		snap:      jobSnapshot{prompt: js.prompt, workDir: js.workDir, fresh: js.freshContext},
	}
	// No pause notice to send: an orphan leaves the streak alone
	// (restartOrphan), so it never auto-pauses its job.
	s.finishRun(rc, runOutcome{
		state: orphanTerminalState, errClass: orphanTerminalErrClass,
		errMsg:        orphanTerminalErrMsg,
		sandbox:       true,
		restartOrphan: true,
	})
}

// removeReconciledPending drops the pending file once reconcile has
// discharged everything it owed for the orphan.
func (s *Scheduler) removeReconciledPending(path string, lg *slog.Logger) {
	if err := s.sandboxState().RemovePending(path); err != nil {
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
	// Fast path: this process wrote the record, so its path is in the index.
	if path := s.lookupSandboxPendingIndex(jobID); path != "" {
		if s.stopOneSandboxPendingFile(jobID, path) {
			s.clearSandboxPendingIndex(jobID, path)
		}
		return
	}
	// Slow path (index miss): a pending file left by a previous process; scan for
	// a JobID match.
	entries, err := s.sandboxState().ListPending()
	if err != nil {
		slog.Warn("cron sandbox: delete-stop pending scan failed", "job_id", jobID, "err", err)
		return
	}
	for _, e := range entries {
		// Bail on shutdown so N×30s StopSession calls don't exhaust gcWaitBudget.
		if s.stopCtx.Err() != nil {
			return
		}
		if e.State != sandboxstore.PendingOK || e.Rec.JobID != jobID {
			continue
		}
		if s.stopOneSandboxPendingFile(jobID, e.Path) {
			s.clearSandboxPendingIndex(jobID, e.Path)
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
	// A record that is gone (the run goroutine just removed it), unreadable or
	// corrupt is skipped. RunID is validated too: it is read from
	// operator-writable disk and flows into slog fields.
	p, state := s.sandboxState().ReadPending(path)
	if state != sandboxstore.PendingOK || p.JobID != jobID || p.RuntimeSessionID == "" || !IsValidID(p.RunID) {
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
	if err := s.sandboxState().RemovePending(path); err != nil {
		lg.Warn("cron sandbox: delete-stop pending remove failed", "err", err)
	}
	return true
}
