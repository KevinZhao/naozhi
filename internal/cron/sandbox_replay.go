package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// ConfirmSandboxRun resolves a §7.4 queue entry as "operator confirmed the run
// already completed" — the `确认已完成` action (RFC §7.4). It does NOT replay:
// the operator has checked the side effect already landed (e.g. the PR was
// pushed), so re-running would duplicate it. Removing the attention record is
// the whole effect — the original CronRun stays failed-transport in history
// (its fate was genuinely unknown to naozhi), but it no longer demands action.
//
// Idempotent: a run that is not in the queue (already resolved, never enqueued)
// returns nil. The runID is shape-validated; an invalid id is the only error.
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
// (RFC §7.3 「重放」 + §7.4 `确认未完成，重放`). It is the capstone of the §6.2
// double-run containment: replaying a transport-failed run is ONLY safe after
// the original microVM is confirmed dead, so this method embeds §6.2 rule 1
// (StopSession-before-replay) as a precondition, not an afterthought.
//
// Flow:
//
//  1. Validate: job exists, is at placement=sandbox, snapshot exists.
//  2. §6.2 rule 1 — if this run is in the attention queue with a runtime
//     session id (transport/orphaned), StopSession FIRST. A failed Stop means
//     the microVM's fate is still unknown → refuse (ErrStopUnconfirmed). The
//     operator retries; StopSession is idempotent.
//  3. Dispatch a fresh run-once microVM with the snapshot's payload (prompt +
//     model), tagged replay_of=<origRunID> so the new CronRun links to the
//     original. The job's CURRENT notify target / label are used (the run
//     belongs to the live job), but the PAYLOAD is the snapshot's (§5.2 "replay
//     re-injects the same input", immune to a since-edited Job.Prompt).
//  4. Resolve the attention record (the incident is now actioned).
//
// Returns the new run's ID on success. The dispatch itself runs synchronously
// up to CAS admission, then the microVM invoke runs in a goroutine registered
// with triggerWG (same lifecycle as TriggerNow) so the HTTP handler returns
// promptly and Stop() drains it.
func (s *Scheduler) ReplaySandboxRun(jobID, origRunID string) (string, error) {
	if !IsValidID(jobID) || !IsValidID(origRunID) {
		return "", errInvalidAttentionID
	}

	// Gate Add-before-Wait against a concurrent Stop (mirrors TriggerNow's
	// R20260610-085718-LB-7 reasoning): read s.stopped under the same RLock
	// that snapshots the job so the registration cannot race the drain.
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

	// Read the input snapshot — the payload to re-inject. No snapshot → no
	// payload → cannot replay (§5.2).
	man, found, err := s.SandboxRunSnapshotManifest(jobID, origRunID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoSnapshot
	}
	prompt, perr := s.SandboxRunSnapshotPrompt(man.PromptHash)
	if perr != nil {
		// The manifest exists but the prompt blob is gone (GC'd / corrupt).
		// Without the prompt there is nothing to inject.
		return "", ErrNoSnapshot
	}
	if prompt == "" {
		return "", ErrNoSnapshot
	}

	// §6.2 rule 1: if the original is in the attention queue, the microVM may
	// still be alive. StopSession FIRST; only a confirmed Stop unlocks replay.
	//
	// FAIL-CLOSED on a read error: a corrupt/torn attention file (writeSandbox-
	// Attention is a plain WriteFile, so a crash mid-write leaves a truncated
	// record) or a transient os.ReadFile fault means we CANNOT confirm the
	// original microVM's fate. Proceeding would skip the Stop and risk the
	// double-run the whole containment exists to prevent (review PR-6 H1), so
	// refuse — same operator-retry semantics as a failed Stop (StopSession is
	// idempotent; once the record reads cleanly the retry completes).
	rec, qok, qerr := s.getSandboxAttention(origRunID)
	if qerr != nil {
		slog.Error("cron sandbox: replay refused — attention record unreadable, microVM fate unknown",
			"job_id", jobID, "orig_run_id", origRunID, "err", qerr)
		return "", ErrStopUnconfirmed
	}
	if qok && rec.RuntimeSessionID != "" {
		ctx, cancel := context.WithTimeout(s.stopCtx, 30*time.Second)
		stopErr := s.sandbox.StopSession(ctx, rec.RuntimeSessionID)
		cancel()
		if stopErr != nil {
			slog.Error("cron sandbox: replay refused — pre-replay Stop unconfirmed",
				"job_id", jobID, "orig_run_id", origRunID, "err", stopErr)
			return "", ErrStopUnconfirmed
		}
		slog.Info("cron sandbox: pre-replay Stop confirmed (§6.2 rule 1)", "job_id", jobID, "orig_run_id", origRunID)
	}

	// Register the replay goroutine with triggerWG before returning so a
	// concurrent Stop() drains it (same contract as TriggerNow). The Add MUST
	// happen under the s.stopped RLock: Stop() sets s.stopped before draining
	// triggerWG via Wait(), so an Add outside the lock could land a positive
	// delta from a zero counter concurrently with that Wait — the
	// R20260610-085718-LB-7 (#2012) Add-before-Wait violation that lets the
	// replay goroutine escape the drain barrier (review PR-6 H2). The earlier
	// stopped check (under the snapshot RLock) is now stale — re-check here.
	// Do NOT hold the lock across dispatchReplay: it calls snapshotJob, which
	// re-acquires s.mu.RLock (writer-starvation risk).
	s.mu.RLock()
	if s.stopped.Load() {
		s.mu.RUnlock()
		return "", ErrSchedulerStopped
	}
	s.triggerWG.Add(1)
	s.mu.RUnlock()
	newRunID, derr := s.dispatchReplay(jobCopy, prompt, man.Model, origRunID)
	if derr != nil {
		// CAS lost / generate failed: the goroutine was never spawned, so undo
		// the registration we just added. dispatchReplay only returns an error
		// on these pre-spawn failures; once it spawns it returns (runID, nil).
		s.triggerWG.Done()
		return "", derr
	}

	// The incident is actioned: drop the attention record. Best-effort — a
	// leftover record would only re-surface a now-replayed run in the queue;
	// the operator can confirm-dismiss it. Done AFTER dispatch so a dispatch
	// failure leaves the record in place for a retry.
	if rerr := s.removeSandboxAttention(origRunID); rerr != nil {
		slog.Warn("cron sandbox: replay dispatched but attention record removal failed", "orig_run_id", origRunID, "err", rerr)
	}
	return newRunID, nil
}

// dispatchReplay drives one replay run through the same CAS-admission +
// finalizer + gauge protocol as executeOpt, but injects the SNAPSHOT payload
// (prompt/model) and tags the run replay_of=origRunID. It mirrors executeOpt's
// frame-local defer discipline exactly: the finalizer + gauge-decrement defers
// MUST live in the goroutine frame that owns the run, so executeSandbox's
// finishRun (which calls finalize()) and the defer cooperate the same way.
//
// Returns (newRunID, nil) once the run goroutine is spawned; (–, err) on a
// pre-spawn failure (CAS lost, run-id generation) so the caller can undo its
// triggerWG.Add. The spawned goroutine owns the triggerWG.Done.
func (s *Scheduler) dispatchReplay(j *Job, prompt, model, origRunID string) (string, error) {
	// Admission: the per-job CAS gate. A replay must not run concurrently with
	// a scheduled tick / manual trigger / another replay of the same job — the
	// same overlap invariant executeOpt enforces. execAcquireSlot emits the
	// overlap-skip pair on loss, but for an operator-initiated replay we prefer
	// a clean 409 over a phantom skip frame, so we take the gate directly.
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

	// Snapshot the job's CURRENT routing fields (notify target, label,
	// placement) — the replay belongs to the live job. The PAYLOAD is the
	// snapshot's, injected below; snap.prompt is only used for the run record's
	// stored prompt, which we override to the replayed prompt for fidelity.
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
		defer func() {
			finalizer.finalize()
			metrics.CronRunInflight.Add(-1)
		}()
		// completed flips true only when executeSandbox returns normally —
		// at which point it has already driven finishSandboxRun → emitRunEnded.
		completed := false
		defer func() {
			if r := recover(); r != nil {
				recordTriggerNowPanic(snap.jobID, r)
				// #2064: emitRunStarted fired synchronously in the caller frame
				// above, so a panic that aborts executeSandbox BEFORE it reaches
				// finishSandboxRun → emitRunEnded would leave subscribers with a
				// started(queued) frame and no matching ended frame — the run
				// hangs in "queued" forever. Close the lifecycle here so the
				// dashboard timeline always pairs. Guarded by `completed` so a
				// (practically impossible) panic AFTER a normal finish can never
				// double-emit an ended frame.
				if !completed {
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
				}
			}
		}()
		metrics.CronRunInflight.Add(1)
		s.executeSandbox(sandboxExecArgs{
			job: j, snap: replaySnap, runID: runID, startedAt: startedAt,
			trigger: TriggerManual, prompt: prompt, model: model,
			notifyTo: notifyTo, inflight: inflight, finalizer: finalizer,
			lg:       lg,
			replayOf: origRunID,
		})
		completed = true
	}()
	return runID, nil
}
