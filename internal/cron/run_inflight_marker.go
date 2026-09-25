package cron

import (
	"encoding/json"
	"errors"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// run_inflight_marker.go — restart fate for LOCAL cron runs. Epic H #2546,
// adoption Phase 0.
//
// A local run that was executing when the process went away used to leave no
// trace in history at all. finishRun's shutdown-cancel path sets skipPersist,
// and that one flag gates two different things:
//
//	recordTerminalResult (Job fields: LastRunAt, counters) — correct to skip:
//	    a cancel must not move the job's last-run timestamp
//	appendRun (runs/<jobID>/ history)                      — wrong to skip:
//	    the run DID execute, sometimes for minutes
//
// And a hard kill never reaches finishRun at all, so no amount of care there
// would cover it. Hence a marker file: written when the run starts, removed when
// it reaches any terminal state, and reconciled at the next boot. Any marker
// still present at startup belonged to a run whose process died, so it becomes a
// canceled record with ErrClassInterrupted.
//
// This is deliberately the same shape as sandbox_pending.go, which has had
// restart reconciliation since agentcore-cloud-sandbox §6.5 — the local path was
// simply never given one. The two stay separate files because they answer
// different questions: a sandbox orphan needs its microVM stopped over the
// network, while a local orphan's process is already gone and only its history
// row is missing.

// runInflightMarker is the on-disk record for a local run in flight, at
// <store-dir>/runinflight/<runID>.json.
type runInflightMarker struct {
	JobID       string      `json:"job_id"`
	RunID       string      `json:"run_id"`
	Trigger     TriggerKind `json:"trigger,omitempty"`
	StartedAtMS int64       `json:"started_at_ms"`
	Prompt      string      `json:"prompt,omitempty"`
	WorkDir     string      `json:"work_dir,omitempty"`
	Fresh       bool        `json:"fresh,omitempty"`
	// Attempts counts boots that already tried to ADOPT this marker (#2712 PR
	// B). Zero (and absent, so pre-adoption markers need no migration) means
	// never tried; at maxAdoptAttempts the reconciler stops adopting and
	// records interrupted, so a marker that somehow crashes its adoption can
	// cost at most one extra boot — never the unbounded crash loop Phase 0's
	// remove-before-record was protecting against (#2751).
	Attempts int `json:"attempts,omitempty"`
}

// runInflightDir resolves the marker directory ("" when persistence is disabled,
// which is every store-less test fixture).
func (s *Scheduler) runInflightDir() string {
	return s.stateSubtree("runinflight")
}

// writeRunInflightMarker persists the marker and returns its path, or "" when
// persistence is off or the write failed. Best-effort by design: a marker that
// cannot be written costs a missing history row on the next crash, which must
// not stop the run itself.
func (s *Scheduler) writeRunInflightMarker(m runInflightMarker, lg *slog.Logger) string {
	dir := s.runInflightDir()
	if dir == "" {
		return ""
	}
	// Symlink-guarded create, same reason as the sandbox pending dir (#2166): a
	// planted `<stateDir>/runinflight → /elsewhere` must not redirect where the
	// next boot looks, nor where this write lands.
	if err := s.mkdirStateSubtree(dir); err != nil {
		lg.Warn("cron: run-inflight dir create failed; an interrupted run will not appear in history", "err", err)
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		lg.Warn("cron: run-inflight marshal failed", "err", err)
		return ""
	}
	// runID is scheduler-generated hex — path-safe by construction.
	path := filepath.Join(dir, m.RunID+".json")
	// Atomic: a truncated marker from a crash mid-write would be dropped as
	// corrupt at reconcile, which is the outcome the marker exists to prevent.
	if err := osutil.WriteFileAtomic(path, b, 0o600); err != nil {
		lg.Warn("cron: run-inflight write failed; an interrupted run will not appear in history", "err", err)
		return ""
	}
	return path
}

// removeRunInflightMarker drops the marker for runID. Called from finishRun for
// every terminal state, including the skipPersist ones: the marker's job is to
// say "this run never finished", so any finish at all must clear it.
func (s *Scheduler) removeRunInflightMarker(runID string) {
	dir := s.runInflightDir()
	if dir == "" || runID == "" {
		return
	}
	if err := os.Remove(filepath.Join(dir, runID+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("cron: run-inflight marker remove failed", "run_id", runID, "err", err)
	}
}

// rewriteRunInflightMarker persists an updated marker in place (adoption bumps
// Attempts before it starts waiting). Reports success; a failure means the
// retry bound cannot be recorded, and the caller must not adopt.
func (s *Scheduler) rewriteRunInflightMarker(path string, m runInflightMarker) bool {
	data, err := json.Marshal(m)
	if err != nil {
		slog.Warn("cron: run-inflight marker re-marshal failed", "path", path, "err", err)
		return false
	}
	if err := osutil.WriteFileAtomic(path, data, 0o600); err != nil {
		slog.Warn("cron: run-inflight marker rewrite failed", "path", path, "err", err)
		return false
	}
	return true
}

// reconcileRunInflight is the startup pass: every marker left on disk is a run
// whose process died mid-flight. Each becomes a canceled record with
// ErrClassInterrupted so the dashboard shows what happened, and the marker
// is removed either way — a marker that cannot be turned into a record must
// still not be reconciled again on every boot.
//
// Runs are appended through appendRun, the same path a live finish uses, so
// retention and the orphan re-check apply identically.
func (s *Scheduler) reconcileRunInflight() {
	s.settleRunInflight(s.claimRunInflight())
}

// inflightSettlement is what claimRunInflight decided and left for the async
// half: the interrupted records to write (run-store IO) and the adoptions to
// await (bounded by adoptionWaitBudget, far too long for Start).
type inflightSettlement struct {
	interrupted []*CronRun
	adoptions   []inflightAdoption
	adopted     int
	dir         string
}

// inflightAdoption is one claimed adoption: the gate is already held, and await
// waits for the adopted CLI turn to settle.
type inflightAdoption struct {
	runID string
	await func()
}

// claimRunInflight is the synchronous half of the reconcile, and Start runs it
// BEFORE s.cron.Start(). The order is the point (#2751): an adoptable run
// claims its job's gate here, so the first tick finds the slot taken and
// overlap-skips, instead of winning the CAS and sending a second turn into the
// same live CLI while this process records the first one interrupted.
//
// Everything here is bounded and stays inside the runinflight directory — one
// ReadDir, one small read per marker, a router map lookup, a gate CAS, and a
// marker rewrite for the (rare) adoptable run. The run-store appends that the
// original "must not block Start" rule was about are left in the settlement.
// Needs the job table loaded: jobStillExists on an empty table would treat
// every marker as an orphan.
func (s *Scheduler) claimRunInflight() inflightSettlement {
	dir := s.runInflightDir()
	if dir == "" || !s.runStoreEnabled() {
		return inflightSettlement{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cron: run-inflight dir read failed; interrupted runs stay invisible", "dir", dir, "err", err)
		}
		return inflightSettlement{}
	}
	out := inflightSettlement{dir: dir}
	now := time.Now()
	// The adoption capability is asserted, never part of SessionRouter: the one
	// production router gains it, every test fake degrades to "nothing to
	// adopt" — the pre-adoption behaviour, and therefore the right default.
	adopter, _ := s.router.(InFlightAdopter)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		m, ok := s.readRunInflightMarker(path)
		if !ok {
			// Corrupt marker: nothing to record, nothing to adopt, and it must
			// not be re-read every boot — the original self-healing rule.
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("cron: corrupt run-inflight marker remove failed", "path", path, "err", err)
			}
			continue
		}
		// The adoption verdict, before anything is deleted: a live mid-turn CLI
		// behind this job's shim means the run may still complete (#2712).
		verdict := AdoptNone
		var run InFlightRun
		if adopter != nil && s.jobStillExists(m.JobID) && m.Attempts < maxAdoptAttempts {
			run, verdict = adopter.AdoptInFlight(sessionkey.CronKey(m.JobID))
		}
		if verdict == AdoptLive {
			// The marker survives into the attempt — it is the only durable
			// record that this run exists — but with its attempt counted, so a
			// crashing adoption costs one extra boot, not a loop.
			m.Attempts++
			if !s.rewriteRunInflightMarker(path, m) {
				// Cannot bound the retries without the counter on disk; fall
				// back to the safe branch rather than risk the loop.
				verdict = AdoptNone
			}
		}
		if verdict == AdoptLive {
			// Claim the job's run slot exactly like a live run would: losing the
			// CAS means a new tick beat us to the job, and two runs at once is
			// worse than recording this one interrupted.
			inflight, won := s.gate.acquire(m.JobID)
			if won {
				inflight.populate(runInflightView{
					RunID:     m.RunID,
					StartedAt: time.UnixMilli(m.StartedAtMS),
					Phase:     PhaseSending,
					Trigger:   m.Trigger,
				})
				mCopy, runCopy := m, run
				out.adoptions = append(out.adoptions, inflightAdoption{
					runID: m.RunID,
					await: func() { s.adoptRun(mCopy, runCopy, inflight) },
				})
				out.adopted++
				continue
			}
			slog.Info("cron: adoption lost the run slot to a fresh tick; recording interrupted",
				"job_id", m.JobID, "run_id", m.RunID)
		}
		// Terminal branches: the marker's story ends here, remove it first
		// (self-healing, as before adoption existed).
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cron: run-inflight marker remove failed during reconcile", "path", path, "err", err)
		}
		if !s.jobStillExists(m.JobID) {
			continue
		}
		errClass, errMsg := ErrClassInterrupted, "naozhi restarted while this run was in flight"
		if verdict == AdoptDriftShutdown {
			// The shim survived the restart but startup shut it down because its
			// argv no longer matched config: the operator's own edit ended this
			// run, and the record says so instead of blaming the restart (#2749).
			errClass, errMsg = ErrClassConfigDrift, "config changed across the restart; the old run's CLI was shut down"
		}
		startedAt := time.UnixMilli(m.StartedAtMS)
		out.interrupted = append(out.interrupted, &CronRun{
			RunID:      m.RunID,
			JobID:      m.JobID,
			State:      RunStateCanceled,
			Trigger:    m.Trigger,
			StartedAt:  startedAt,
			EndedAt:    now,
			DurationMS: now.Sub(startedAt).Milliseconds(),
			Prompt:     osutil.SanitizeForLog(m.Prompt, MaxPromptBytes),
			WorkDir:    m.WorkDir,
			Fresh:      m.Fresh,
			ErrorClass: errClass,
			ErrorMsg:   errMsg,
		})
	}
	return out
}

// settleRunInflight is the async half: it writes the interrupted records and
// starts each claimed adoption's wait, each in its own isolated startup pass.
func (s *Scheduler) settleRunInflight(out inflightSettlement) {
	for _, rec := range out.interrupted {
		s.appendRun(rec)
	}
	for _, a := range out.adoptions {
		s.goStartupPass("run-adoption-"+a.runID, a.await)
	}
	if len(out.interrupted) > 0 || out.adopted > 0 {
		slog.Info("cron: reconciled runs from the previous process",
			"interrupted", len(out.interrupted), "adopted", out.adopted, "dir", out.dir)
	}
}

// readRunInflightMarker loads one marker. ok=false for anything unusable; the
// caller removes the file regardless.
func (s *Scheduler) readRunInflightMarker(path string) (runInflightMarker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("cron: run-inflight marker read failed", "path", path, "err", err)
		return runInflightMarker{}, false
	}
	var m runInflightMarker
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("cron: run-inflight marker parse failed; dropping", "path", path, "err", err)
		return runInflightMarker{}, false
	}
	if m.RunID == "" || m.JobID == "" || m.StartedAtMS <= 0 {
		slog.Warn("cron: run-inflight marker incomplete; dropping", "path", path)
		return runInflightMarker{}, false
	}
	return m, true
}
