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

// sandboxPending is the §6.5 in-flight record, written to
// <store-dir>/sandboxpending/<runID>.json before InvokeAgentRuntime and
// removed after the run reaches a terminal state. Its sole purpose is to
// survive a naozhi restart: the held stream dies with the process, but the
// microVM keeps running — this file is the only handle the next boot has
// to Stop it and close out the run record.
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
	// #2166: route the dir create through the symlink-guarded helper so a
	// planted `<stateDir>/sandboxpending → /elsewhere` cannot silently redirect
	// the §6.5 restart-reconcile handle into an attacker-chosen directory.
	// Degrades to no reconcile handle (like a MkdirAll failure) rather than
	// writing through the symlink.
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
	// Atomic write (tmp→fsync→rename→SyncDir): this file is the ONLY restart
	// reconcile handle to Stop an orphaned microVM (godoc lines 16-21). A
	// crash mid-write under bare os.WriteFile could leave a truncated/empty
	// record that reconcile drops as corrupt → permanent microVM orphan
	// (R20260614-ARCH-1).
	if err := osutil.WriteFileAtomic(path, b, 0o600); err != nil {
		lg.Warn("cron sandbox: pending write failed; restart reconcile unavailable for this run", "err", err)
		return ""
	}
	// R20260616-PERF-001 (#2140): record the live jobID→path mapping so a
	// later DeleteJobByID resolves this run's pending file with one map lookup
	// instead of scanning + unmarshalling every concurrent run's record. The
	// per-job CAS keeps at most one in-flight run per job, so a single entry
	// per key is correct; a re-write for the same job overwrites the (now
	// stale) prior path.
	s.setSandboxPendingIndex(p.JobID, path)
	return path
}

// setSandboxPendingIndex records jobID→path for the §6.5 in-flight record.
// sandboxPendingIndex is allocated in NewScheduler (scheduler.go) and is the
// only construction path that reaches the sandbox write seam, so the map is
// guaranteed non-nil here — no nil guard needed [R202606e-GO-001].
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

// reconcileSandboxPending is the §6.5 startup pass: every pending file is
// an orphaned run — the previous process died while holding its stream.
// For each: Stop the microVM (idempotent; it may have finished or been
// idle-burned long ago), close the run record as failed-transport with an
// orphaned marker, and drop the file. Runs asynchronously from Start()
// (mirrors the cold-start runs/ GC) — Stop calls are network I/O and must
// not block scheduler startup.
//
// The terminal record goes through finishRun's three-write protocol with a
// synthetic started event first, so subscribers see a consistent
// started→ended pair (the original started frame died with the previous
// process — same rationale as emitSyntheticSkipped).
//
// R20260616-PERF-006 (#2142): the cheap validate/drop-corrupt pass stays
// serial (local I/O), then the surviving orphans' Stops are fanned out across
// a bounded worker pool (sandboxReconcileWorkers). Each orphan's StopSession
// is an independent ~30s network call, so serial N×30s on a slow upstream
// could stall the reconcile pass for minutes; the pool caps that to
// ⌈N/workers⌉×30s while bounding peak in-flight Stops. reconcileOneSandboxOrphan
// is concurrency-safe: every shared-state touch goes through s.mu (RLock
// snapshot + finishRun's own re-lock) or atomic metrics counters.
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
		// Mirrors trimAllCtx's inter-entry ctx.Err() check (scheduler.go).
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
			// Corrupt or tampered record (RunID/JobID must be scheduler-
			// generated hex — they flow into run-record paths and the
			// broadcast, so shape-validate before use). StartedAtMS<=0
			// (R20260614-GO-003) is equally corrupt: time.UnixMilli on a
			// zero/negative value yields a 1970 (or pre-epoch) StartedAt that
			// flows into CronRun.StartedAt and an astronomical DurationMS,
			// wrecking the dashboard timeline — drop the record. Remove so it
			// does not re-warn on every boot.
			// RuntimeSessionID=="" (R20260615-030459-COR-002): a pending record
			// without a runtime session id cannot be reconciled — reconcile would
			// skip the StopSession block yet still call finishRun and
			// remove the file, breaking §6.2 containment. Treat as corrupt:
			// drop+warn, aligned with stopSandboxRunsForJob's guard.
			slog.Warn("cron sandbox: corrupt pending record dropped", "file", osutil.SanitizeForLog(e.Name(), 256), "err", err)
			_ = os.Remove(path)
			continue
		}
		orphans = append(orphans, orphan{p: p, path: path})
	}

	if len(orphans) == 0 {
		return
	}
	// Serial when there is nothing to parallelise — avoid the goroutine +
	// channel plumbing for the common single-orphan case.
	if len(orphans) == 1 {
		// R202606-CR-002: honor shutdown before the (up to sandboxStopTimeout)
		// Stop, mirroring the per-orphan ctx.Err() gate the parallel path applies
		// at line 241. Without this a SIGTERM during a single-orphan reconcile
		// still blocks ~30s on the Stop, slowing shutdown.
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
				// Per-orphan shutdown bail: a Stop() racing reconcile cancels
				// stopCtx; stop dispatching new Stops so we don't exhaust the
				// gcWaitBudget. reconcileOneSandboxOrphan also re-checks via its
				// WithTimeout(stopCtx, …) so an in-flight Stop unblocks too.
				if s.stopCtx.Err() != nil {
					continue
				}
				s.reconcileOneSandboxOrphan(o.p, o.path)
			}
		}()
	}
	// R202606e-GO-002: select on stopCtx while feeding the unbuffered jobs
	// channel. Without this a SIGTERM during a multi-orphan reconcile cannot
	// unblock the send side: workers stop dispatching Stops (line 236) but keep
	// draining, while the sender stays parked on `jobs <- o` until every orphan
	// is handed off — so close(jobs)/wg.Wait() (and the gcWaitBudget) are held
	// hostage by the remaining sends. Bail out of feeding once shutdown begins.
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

// orphanDecision is what reconcile owes a single §6.5 orphan once the Stop
// and the terminal-record probe have been observed. It is the output of the
// pure classifier classifyOrphanPending; reconcileOneSandboxOrphan only
// executes it. Centralising the five historical keep/remove early returns
// here (#2172 / R202606-ARCH-3) turns the §6.2 containment state machine
// from five inline comments into one table-tested function.
type orphanDecision uint8

const (
	// orphanKeepPending: the microVM's fate is unknown (§6.2 not satisfied)
	// or cannot be confirmed from this process — leave the pending file so
	// the NEXT start retries. Nothing else is touched.
	orphanKeepPending orphanDecision = iota
	// orphanRemoveOnly: the Stop confirmed but the run is ALREADY terminal
	// on disk (an in-process transport failure whose Stop had not confirmed
	// drove finishRun before the process died, #2054). Only the microVM Stop
	// and the pending file were owed — a second finish would double-count
	// RunCounters + metrics and broadcast a phantom lifecycle.
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
	// orphanReasonSandboxUnconfigured: cron.sandbox removed between restarts
	// — no Stop primitive exists this boot (review §6.5 F1).
	orphanReasonSandboxUnconfigured
	// orphanReasonInvalidSessionID: RuntimeSessionID read from an operator-
	// writable file fails the production-format check (R20260613-SEC-2). An
	// empty id is folded in here too: without a handle §6.2 cannot be
	// satisfied, so keeping the file is the only containment-preserving move
	// (reconcileSandboxPending already drops "" as corrupt upstream, COR-002).
	orphanReasonInvalidSessionID
	// orphanReasonStopFailed: StopSession returned an error — fate unknown.
	orphanReasonStopFailed
	// orphanReasonAlreadyTerminal: runs/<job>/<run>.json has EndedAt set.
	orphanReasonAlreadyTerminal
	// orphanReasonProbeTransient: s.Run failed with something other than
	// fs.ErrNotExist / ErrCorruptRun (EIO / ESTALE / EACCES …, #2149) — the
	// record may be terminal, so finishing now could double-count.
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

// classifyOrphanPending is the pure §6.2/§6.5 containment state machine for
// one orphan. Rules, in order (first match wins):
//
//  1. sandbox not configured            → Keep   (F1: retry handle must survive)
//  2. RuntimeSessionID invalid or empty → Keep   (SEC-2: cannot confirm fate)
//  3. Stop returned an error            → Keep   (§6.2 rule 1 unsatisfied)
//  4. run record present and EndedAt≠0 → RemoveOnly (#2054: already finished)
//  5. record probe failed transiently   → Keep   (#2149: may be terminal)
//  6. otherwise                         → RemoveAfterFinish
//
// Rule 5 deliberately lets fs.ErrNotExist (no record) and ErrCorruptRun
// (record can never be confirmed terminal) fall through to rule 6.
// No I/O, no locks: TestClassifyOrphanPending covers every branch.
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
// verdict at the historical level for that reason. RemoveAfterFinish has no
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
	// R20260615-030459-COR-001 / R202606-CR-001 (#2156): re-check job existence
	// under RLock ONCE before any subscriber-visible write. The snapshot was
	// taken under a lock we have since released; a concurrent DeleteJobByID
	// that ran in the gap deletes the job + sweeps the attention queue.
	// Without this re-check we would (a) leave a ghost attention card whose
	// replay ErrJobNotFound's, and (b) broadcast a phantom
	// cron_run_started/ended pair for a job the dashboard no longer shows.
	// The SAME boolean feeds the attention write and finishOrphanRun so the
	// two agree on whether the job still exists. j!=nil but
	// jobExists==false ⇒ deleted in the gap: metrics-only path (counters
	// stay balanced, nothing broadcast).
	jobExists := j != nil && s.jobExists(p.JobID)
	if jobExists {
		s.maybeEnqueueOrphanAttention(p, js, lg)
	}
	s.finishOrphanRun(p, js, j, jobExists, lg)
	removeReconciledPending(path, lg)
}

// orphanJobSnapshot is the subset of *Job the orphan finish needs, copied
// under s.mu.RLock (R20260613-GOLANG-001: UpdateJob mutates *Job fields in
// place under s.mu.Lock, so any lock-free read is a data race).
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

// maybeEnqueueOrphanAttention applies §6.2 rule 3 + §7.4 to a live-job
// orphan: a side-effecting job enters the human confirmation queue. The
// microVM was Stopped, but it may have completed and produced its side
// effect (PR push, etc.) before naozhi died — only a human can tell. A
// side-effect-free orphan is safe to leave as a plain failed-transport
// record (it re-runs next tick). RuntimeSessionID is already spent (we
// Stopped it); kept on the record for symmetry — the queue's replay action
// re-Stops idempotently.
//
// #2119: an in-process transport failure may have ALREADY enqueued an
// attention record for this runID (reason=transport) before the process
// died. writeSandboxAttention uses WriteFileAtomic to the same <runID>.json
// path, so an unconditional write would CLOBBER it and downgrade the reason
// from "transport" (stream lost) to "orphaned" (restart) — misleading the
// operator about what actually happened. Probe first; write only when no
// record exists. A read error (qerr) is treated as "may exist" → skip,
// preserving any prior reason (the failed-transport CronRun still warns).
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
// accounting (#2172: the former j!=nil vs nil-job metrics paths had to be
// mirrored by hand and drifted — R20260613-CR-2, R20260614-GO-001).
//
// jobExists=true — full protocol: synthetic started frame + finishRun
// (persist → bumpRunStateMetrics → broadcast → CronRunEndedTotal).
//
// jobExists=false — metrics-only mirror of that protocol with the broadcast
// halves removed, in the SAME order the live path produces them:
// CronRunStartedTotal (what emitRunStarted would bump) → per-state buckets
// via bumpRunStateMetrics (what finishRun bumps after persist settles; no
// persist is attempted here, so no gate — same as finishRun's skipPersist
// branch) → CronRunEndedTotal (finishRun's final statement). The job was
// deleted while naozhi was down (j==nil, R20260613-GOLANG-004) or in the
// snapshot→finish gap (#2156): there is no live job to attach a record to,
// and a started/ended pair for a job the dashboard already dropped would be
// a phantom lifecycle. The run did start and end (failed), so the
// Started/Ended balance (/health, runstore.go in-flight gauge) and the
// per-state buckets must still advance by one — never by hand-writing the
// per-state Add. TestReconcileOrphan_TerminalCounterParity pins that both
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
	// finalizer carries a NIL inflight deliberately: the orphan belongs to
	// the PREVIOUS process — this process's CAS gate was never taken for it.
	// The reconcile goroutine runs after cron.Start(), so the same job's
	// run-B may be live RIGHT NOW holding the gate; a finalizer bound to
	// s.jobInflight(jobID) would reset run-B's view and Store(false) its
	// gate, letting a third tick double-run. finalize() no-ops on nil
	// inflight, which is exactly right here.
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

// stopSandboxRunsForJob terminates any in-flight sandbox microVM(s) for a
// job being deleted, closing the Phase 1 gap (executeSandbox godoc): until
// now DeleteJobByID left a live run to finish or hit maxLifetime, burning
// cloud cost and possibly producing side effects the operator no longer
// wants. The §6.5 pending record carries the runtime session id, so delete
// can now Stop the microVM directly.
//
// Runs lock-free from deleteJobPostCleanup. Best-effort and idempotent:
//
//   - R20260616-PERF-001 (#2140): the common case resolves the job's in-flight
//     pending file with a single map lookup (sandboxPendingIndex, kept write-
//     authoritative by writeSandboxPending / the terminal remove) instead of an
//     os.ReadDir + per-file ReadFile/unmarshal over EVERY concurrent run's
//     record. Only falls back to the full dir scan on an index miss — i.e. for
//     a pending file written by a PREVIOUS process that this boot's index never
//     saw (those are normally drained by reconcileSandboxPending at startup, but
//     a delete that races reconcile must still find them).
//   - StopSession is idempotent server-side and maps ResourceNotFound→nil
//     (adapter), so a run that finished + removed its pending file between
//     our lookup and the Stop is harmless.
//   - Removes the pending file after a confirmed Stop so startup reconcile
//     does not later re-Stop a session for a job that no longer exists. On
//     Stop failure the file is KEPT (mirrors reconcile / §6.2): the microVM
//     fate is unknown and the next startup must retry.
//
// No terminal CronRun is written here: the in-flight run's own goroutine is
// still holding the stream and will reach finishRun (which no-ops the
// persist for the now-deleted job via recordTerminalResult's jobs[id]
// re-check). Writing a record here would race that goroutine.
func (s *Scheduler) stopSandboxRunsForJob(jobID string) {
	if s.sandbox == nil {
		return // sandbox placement not configured — nothing could be in flight
	}
	dir := s.sandboxPendingDir()
	if dir == "" {
		return
	}
	// Fast path: this process wrote the pending record, so its path is in the
	// index — one lookup + one ReadFile, no full-dir scan.
	if path := s.lookupSandboxPendingIndex(jobID); path != "" {
		if s.stopOneSandboxPendingFile(jobID, path) {
			s.clearSandboxPendingIndex(jobID, path)
		}
		return
	}
	// Slow path (index miss): a pending file may have been left by a previous
	// process. Scan the dir for a JobID match. Bounded by the live orphan
	// count, and only taken when the in-memory index has nothing for jobID.
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cron sandbox: delete-stop pending scan failed", "job_id", jobID, "err", err)
		}
		return
	}
	for _, e := range entries {
		// R20260613-GO-002: bail on shutdown so N×30s StopSession calls don't
		// exhaust gcWaitBudget. Mirrors reconcileSandboxPending's inter-entry
		// guard (line 105).
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
	// R20260613-LOGIC-2: validate RunID in addition to JobID/RuntimeSessionID.
	// p.RunID is read from operator-writable disk and flows into slog fields
	// below — without validation a tampered pending file can inject control
	// characters or oversized strings into structured logs. Mirrors the same
	// guard in reconcileSandboxPending.
	if err := json.Unmarshal(raw, &p); err != nil || p.JobID != jobID || p.RuntimeSessionID == "" || !IsValidID(p.RunID) {
		return false
	}
	// R20260613-SEC-2: validate RuntimeSessionID read from disk before passing
	// to StopSession. On invalid format: log-warn and skip (file is kept —
	// startup reconcile retries on next boot).
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
