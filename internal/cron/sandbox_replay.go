package cron

import (
	"context"
	"log/slog"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
)

// ConfirmSandboxRun resolves a queue entry as "operator confirmed the run
// already completed" (确认已完成). It does NOT replay: the side effect already
// landed, so re-running would duplicate it. Removing the attention record is
// the whole effect — the original CronRun stays failed-transport in history.
// Idempotent: a run not in the queue returns nil; an invalid id is the only error.
func (s *Scheduler) ConfirmSandboxRun(runID string) error {
	if !IsValidID(runID) {
		return errInvalidAttentionID
	}
	if err := s.removeSandboxAttention(runID); err != nil {
		return err
	}
	slog.Info("cron sandbox: run confirmed done via dashboard; removed from attention queue", "run_id", runID)
	return nil
}

// ReplaySandboxRun re-executes a sandbox run from its persisted input snapshot
// (重放 / 确认未完成，重放). Replaying a transport-failed run is ONLY safe after
// the original microVM is confirmed dead, so StopSession-before-replay is a
// precondition: if the run is in the attention queue with a runtime session
// id, a failed Stop refuses with ErrStopUnconfirmed (operator retries; Stop is
// idempotent). The new run uses the job's CURRENT notify target / label but the
// snapshot's PAYLOAD (prompt + model), tagged replay_of=<origRunID>, and the
// attention record is resolved afterwards. Returns the new run's ID; dispatch
// is synchronous up to CAS admission, then the invoke runs in a goroutine
// registered with triggerWG so Stop() drains it.
func (s *Scheduler) ReplaySandboxRun(jobID, origRunID string) (string, error) {
	if !IsValidID(jobID) || !IsValidID(origRunID) {
		return "", errInvalidAttentionID
	}

	// Read s.stopped under the same RLock that snapshots the job so the triggerWG
	// registration cannot race Stop's drain.
	s.mu.RLock()
	if s.stopped.Load() {
		s.mu.RUnlock()
		return "", ErrSchedulerStopped
	}
	j, ok := s.jobs[jobID]
	if !ok {
		s.mu.RUnlock()
		return "", ErrJobNotFound
	}
	if !placementIsSandbox(j.Placement) {
		s.mu.RUnlock()
		return "", ErrJobNotSandbox
	}
	jobCopy := j // pointer is stable; snapshotJob re-reads under lock below
	s.mu.RUnlock()

	if s.sandbox == nil {
		return "", ErrSandboxUnavailable
	}

	// No snapshot → no payload → cannot replay.
	man, found, err := s.SandboxRunSnapshotManifest(jobID, origRunID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoSnapshot
	}
	prompt, perr := s.SandboxRunSnapshotPrompt(man.PromptHash)
	if perr != nil {
		// Manifest exists but the prompt blob is gone (GC'd / corrupt): nothing to inject.
		return "", ErrNoSnapshot
	}
	if prompt == "" {
		return "", ErrNoSnapshot
	}
	// The blob may predate the current containsCronUnsafe / validateCronPrompt
	// allowlist and write-edge validation does not re-run on reads, so scrub here
	// before the payload is injected into the microVM (#2319). Idempotent on
	// already-clean prompts.
	prompt = osutil.SanitizeForLog(prompt, MaxPromptBytes)

	// If the original is in the attention queue the microVM may still be alive:
	// StopSession FIRST. FAIL-CLOSED on a read error — a torn/corrupt attention
	// file means the original's fate cannot be confirmed, and proceeding would skip
	// the Stop and risk the double-run this containment exists to prevent.
	rec, qok, qerr := s.getSandboxAttention(origRunID)
	if qerr != nil {
		slog.Error("cron sandbox: replay refused — attention record unreadable, microVM fate unknown",
			"job_id", jobID, "orig_run_id", origRunID, "err", qerr)
		return "", ErrStopUnconfirmed
	}
	if qok && rec.RuntimeSessionID != "" {
		// RuntimeSessionID comes from an operator-writable file; an invalid format
		// refuses replay (fate stays unknown).
		if !isValidRuntimeSessionID(rec.RuntimeSessionID) {
			slog.Error("cron sandbox: replay refused — attention record has invalid RuntimeSessionID format",
				"job_id", jobID, "orig_run_id", origRunID, "runtime_session_id", rec.RuntimeSessionID)
			return "", ErrStopUnconfirmed
		}
		ctx, cancel := context.WithTimeout(s.stopCtx, sandboxStopTimeout)
		stopErr := s.sandbox.StopSession(ctx, rec.RuntimeSessionID)
		cancel()
		if stopErr != nil {
			slog.Error("cron sandbox: replay refused — pre-replay Stop unconfirmed",
				"job_id", jobID, "orig_run_id", origRunID, "err", stopErr)
			return "", ErrStopUnconfirmed
		}
		slog.Info("cron sandbox: pre-replay Stop confirmed (§6.2 rule 1)", "job_id", jobID, "orig_run_id", origRunID)
	}

	// triggerWG.Add MUST happen under the s.stopped RLock: Stop() sets s.stopped
	// before Wait(), so an Add outside the lock could land on a zero counter
	// concurrently with the drain and let the goroutine escape (#2012). The
	// earlier stopped check is stale by now — re-check. Do NOT hold the lock
	// across dispatchReplay: it re-acquires s.mu.RLock via snapshotJob.
	s.mu.RLock()
	if s.stopped.Load() {
		s.mu.RUnlock()
		return "", ErrSchedulerStopped
	}
	s.triggerWG.Add(1)
	s.mu.RUnlock()
	newRunID, derr := s.dispatchReplay(jobCopy, prompt, man.Model, origRunID)
	if derr != nil {
		// Pre-spawn failure (CAS lost / generate failed): undo the registration; once
		// spawned, the goroutine owns Done.
		s.triggerWG.Done()
		return "", derr
	}

	// The incident is actioned: drop the attention record. Best-effort, and done
	// AFTER dispatch so a dispatch failure leaves the record for a retry.
	if rerr := s.removeSandboxAttention(origRunID); rerr != nil {
		slog.Warn("cron sandbox: replay dispatched but attention record removal failed", "orig_run_id", origRunID, "err", rerr)
	}
	return newRunID, nil
}

// dispatchReplay drives one replay run through the same CAS-admission +
// finalizer + gauge protocol as executeOpt (via runScaffold), but injects the
// SNAPSHOT payload (prompt/model) and tags the run replay_of=origRunID.
// Returns (newRunID, nil) once the run goroutine is spawned; (–, err) on a
// pre-spawn failure (CAS lost, run-id generation) so the caller can undo its
// triggerWG.Add. The spawned goroutine owns the triggerWG.Done.
func (s *Scheduler) dispatchReplay(j *Job, prompt, model, origRunID string) (string, error) {
	// Per-job CAS gate: a replay must not overlap a tick / manual trigger / another
	// replay. Taken directly (not via execAcquireSlot) so an operator-initiated
	// replay gets a clean 409 instead of a phantom overlap-skip frame.
	gate := s.jobGateLock(j.ID)
	gate.Lock()
	inflight := s.jobInflight(j.ID)
	won := inflight.running.CompareAndSwap(false, true)
	gate.Unlock()
	if !won {
		return "", ErrReplayInFlight
	}

	runID, err := generateRunID()
	if err != nil {
		// Release the gate we just won — no goroutine will run to finalize it.
		inflight.running.Store(false)
		return "", err
	}

	startedAt := s.now()
	inflight.populate(runInflightView{
		RunID:     runID,
		StartedAt: startedAt,
		Phase:     PhaseQueued,
		Trigger:   TriggerManual,
	})

	// Snapshot the job's CURRENT routing fields (notify target, label, placement);
	// the PAYLOAD is the snapshot's, injected below.
	snap := s.snapshotJob(j)
	notifyTo := s.resolveNotifyTarget(snap.platName, snap.chatID, snap.notifyPlat, snap.notifyChat, snap.notify)

	s.emitRunStarted(RunStartedEvent{
		JobID:     snap.jobID,
		RunID:     runID,
		StartedAt: startedAt,
		Trigger:   TriggerManual,
		Fresh:     snap.fresh,
	})

	lg := slog.With("job_id", snap.jobID, "run_id", runID, "replay_of", origRunID)
	lg.Info("cron sandbox: replaying run from input snapshot")

	finalizer := &runFinalizer{inflight: inflight}

	// Override the snapshot's prompt with the replayed payload so the new run
	// record stores what was actually injected (§5.2 fidelity). Model likewise.
	replaySnap := snap
	replaySnap.prompt = prompt

	go func() {
		defer s.triggerWG.Done()
		// runScaffold owns the finalizer/gauge defers and the completed-guarded panic
		// recover (shared with executeOpt); onPanic runs only after the scaffold has
		// finalized (#2174, #2094).
		runScaffold{
			finalizer: finalizer,
			jobID:     snap.jobID,
			onPanic: func(any) {
				// emitRunStarted fired synchronously above, so a panic before
				// finishSandboxRun → emitRunEnded would leave the run "queued" forever;
				// close the lifecycle here (#2064).
				s.emitRunEnded(RunEndedEvent{
					JobID:      snap.jobID,
					RunID:      runID,
					State:      RunStateFailed,
					StartedAt:  startedAt,
					EndedAt:    s.now(),
					Trigger:    TriggerManual,
					ErrorClass: ErrClassSandboxFailed,
					ErrorMsg:   "sandbox replay panicked before terminal record",
				})
				metrics.CronRunEndedTotal.Add(1) // completed==false guarantees no double-count
				// This path bypasses finishRun → bumpRunStateMetrics, so bump the per-state
				// + sandbox failure counters itself or they undercount vs CronRunEndedTotal (#2223).
				metrics.CronRunFailedTotal.Add(1)
				metrics.CronSandboxRunFailedTotal.Add(1)
			},
		}.run(func() {
			s.executeSandbox(sandboxExecArgs{
				job: j, snap: replaySnap, runID: runID, startedAt: startedAt,
				trigger: TriggerManual, prompt: prompt, model: model,
				notifyTo: notifyTo, inflight: inflight, finalizer: finalizer,
				lg:       lg,
				replayOf: origRunID,
			})
		})
	}()
	return runID, nil
}
