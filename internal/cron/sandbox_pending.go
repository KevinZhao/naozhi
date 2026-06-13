package cron

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	if s.storePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.storePath), "sandboxpending")
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if err := os.WriteFile(path, b, 0o600); err != nil {
		lg.Warn("cron sandbox: pending write failed; restart reconcile unavailable for this run", "err", err)
		return ""
	}
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
			slog.Warn("cron sandbox: pending read failed; skipping", "file", e.Name(), "err", err)
			continue
		}
		var p sandboxPending
		if err := json.Unmarshal(raw, &p); err != nil || !IsValidID(p.RunID) || !IsValidID(p.JobID) {
			// Corrupt or tampered record (RunID/JobID must be scheduler-
			// generated hex — they flow into run-record paths and the
			// broadcast, so shape-validate before use). Remove so it does
			// not re-warn on every boot.
			slog.Warn("cron sandbox: corrupt pending record dropped", "file", e.Name(), "err", err)
			_ = os.Remove(path)
			continue
		}
		s.reconcileOneSandboxOrphan(p, path)
	}
}

// reconcileOneSandboxOrphan handles a single §6.5 orphan: Stop, terminal
// record, file removal. Stop failure keeps the file so the NEXT start
// retries — until a Stop is confirmed the microVM's fate is unknown and
// the §6.2 containment is not satisfied.
func (s *Scheduler) reconcileOneSandboxOrphan(p sandboxPending, path string) {
	lg := slog.With("job_id", p.JobID, "run_id", p.RunID)
	lg.Warn("cron sandbox: reconciling orphaned run from previous process")

	if s.sandbox == nil {
		// Sandbox config absent at reconcile time (removed between
		// restarts). KEEP the file: the §6.2 retry handle must survive
		// until a boot where the Stop primitive exists and confirms —
		// removing it here would orphan the microVM with no future
		// containment (review §6.5 F1). Re-warns each boot by design.
		lg.Warn("cron sandbox: orphaned run found but sandbox not configured; keeping pending record until config returns")
		return
	}
	if p.RuntimeSessionID != "" {
		ctx, cancel := context.WithTimeout(s.stopCtx, 30*time.Second)
		err := s.sandbox.StopSession(ctx, p.RuntimeSessionID)
		cancel()
		if err != nil {
			lg.Error("cron sandbox: orphan Stop failed; keeping pending record for next start", "err", err)
			return
		}
	}

	// #2054: an in-process transport failure whose Stop did NOT confirm
	// (sandbox.go default branch) ALREADY drove finishRun → addRun +
	// metrics + a durable runs/{jobID}/{runID}.json terminal record, yet
	// deliberately KEEPS the pending file so this reconcile can retry the
	// Stop. If we re-finish that same runID here we double-count the durable
	// RunCounters, re-bump CronRunEnded/Failed/StartedTotal, and emit a
	// phantom started→ended lifecycle to subscribers. So once the Stop has
	// been (re-)confirmed above, check whether the run is already terminal on
	// disk; if so, only the microVM Stop + pending-file removal were owed —
	// skip the second finishRun entirely.
	if rec, err := s.Run(p.JobID, p.RunID); err == nil && rec != nil && !rec.EndedAt.IsZero() {
		lg.Info("cron sandbox: orphan already finished in-process; skipping duplicate finish",
			"state", rec.State)
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			lg.Warn("cron sandbox: reconciled pending remove failed", "err", rmErr)
		}
		return
	}

	// Terminal record. The job may have been deleted while we were down —
	// finishRun's recordTerminalResult re-checks s.jobs[id] and no-ops the
	// persist; the broadcast pair still closes subscriber timelines.
	s.mu.RLock()
	j := s.jobs[p.JobID]
	s.mu.RUnlock()
	startedAt := time.UnixMilli(p.StartedAtMS)
	msg := "naozhi restarted while the run was in flight; microVM terminated by startup reconcile"
	if j != nil {
		// §6.2 rule 3 + §7.4: a side-effecting orphan enters the human
		// confirmation queue. The microVM was Stopped above, but it may have
		// completed and produced its side effect (PR push, etc.) before naozhi
		// died — only a human can tell. A side-effect-free orphan is safe to
		// leave as a plain failed-transport record (it re-runs next tick).
		// RuntimeSessionID is already spent (we Stopped it); kept on the record
		// for symmetry — the queue's replay action re-Stops idempotently.
		if j.SideEffects != nil && *j.SideEffects {
			s.writeSandboxAttention(sandboxAttention{
				JobID:            p.JobID,
				RunID:            p.RunID,
				RuntimeSessionID: p.RuntimeSessionID,
				Reason:           attentionReasonOrphaned,
				JobLabel:         jobTitleOrFallback(j),
				StartedAtMS:      p.StartedAtMS,
				CreatedAtMS:      s.attentionNowMS(),
			}, lg)
		}
		// Synthetic started so subscribers get a paired lifecycle (the real
		// started frame belonged to the previous process's broadcaster).
		s.emitRunStarted(RunStartedEvent{
			JobID:     p.JobID,
			RunID:     p.RunID,
			StartedAt: startedAt,
			Trigger:   runtelemetry.TriggerScheduled,
			Fresh:     j.FreshContext,
		})
		// finalizer carries a NIL inflight deliberately: the orphan belongs
		// to the PREVIOUS process — this process's CAS gate was never taken
		// for it. The reconcile goroutine runs after cron.Start(), so the
		// same job's run-B may be live RIGHT NOW holding the gate; a
		// finalizer bound to s.jobInflight(jobID) would reset run-B's view
		// and Store(false) its gate, letting a third tick double-run.
		// finalize() no-ops on nil inflight, which is exactly right here.
		s.finishRun(finishArgs{
			job: j, runID: p.RunID, startedAt: startedAt,
			trigger: runtelemetry.TriggerScheduled,
			state:   RunStateFailed, errClass: ErrClassSandboxTransport,
			errMsg: msg,
			prompt: j.Prompt, workDir: j.WorkDir, fresh: j.FreshContext,
			finalizer: &runFinalizer{},
		})
	} else {
		lg.Info("cron sandbox: orphan's job no longer exists; closing record file only")
	}

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
//   - Scans the pending dir for records whose JobID matches (a job has at
//     most one in-flight run under the per-job CAS, but scan-by-jobID is
//     robust to any future fan-out and needs no jobID→runID index).
//   - StopSession is idempotent server-side and maps ResourceNotFound→nil
//     (adapter), so a run that finished + removed its pending file between
//     our scan and the Stop is harmless.
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cron sandbox: delete-stop pending scan failed", "job_id", jobID, "err", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // benign: the run goroutine may have just removed it
		}
		var p sandboxPending
		if err := json.Unmarshal(raw, &p); err != nil || p.JobID != jobID || p.RuntimeSessionID == "" {
			continue
		}
		lg := slog.With("job_id", jobID, "run_id", p.RunID)
		lg.Info("cron sandbox: deleting job with in-flight run; stopping microVM")
		ctx, cancel := context.WithTimeout(s.stopCtx, 30*time.Second)
		stopErr := s.sandbox.StopSession(ctx, p.RuntimeSessionID)
		cancel()
		if stopErr != nil {
			// Keep the file: §6.2 — fate unknown until a confirmed Stop.
			// Startup reconcile retries. The deletion itself still proceeds.
			lg.Error("cron sandbox: delete-stop failed; pending record kept for startup reconcile", "err", stopErr)
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			lg.Warn("cron sandbox: delete-stop pending remove failed", "err", err)
		}
	}
}
