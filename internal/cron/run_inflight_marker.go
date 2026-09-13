package cron

import (
	"encoding/json"
	"errors"
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

// reconcileRunInflight is the startup pass: every marker left on disk is a run
// whose process died mid-flight. Each becomes a canceled record with
// ErrClassInterrupted so the dashboard shows what happened, and the marker
// is removed either way — a marker that cannot be turned into a record must
// still not be reconciled again on every boot.
//
// Runs are appended through appendRun, the same path a live finish uses, so
// retention and the orphan re-check apply identically.
func (s *Scheduler) reconcileRunInflight() {
	dir := s.runInflightDir()
	if dir == "" || !s.runStoreEnabled() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cron: run-inflight dir read failed; interrupted runs stay invisible", "dir", dir, "err", err)
		}
		return
	}
	now := time.Now()
	recovered := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		m, ok := s.readRunInflightMarker(path)
		// Remove first either way: a corrupt marker carries nothing to record and
		// would otherwise be re-read on every boot.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cron: run-inflight marker remove failed during reconcile", "path", path, "err", err)
		}
		if !ok {
			continue
		}
		// A job deleted while naozhi was down has no history to attach to;
		// appendRun's own orphan guard would drop the record anyway.
		if !s.jobStillExists(m.JobID) {
			continue
		}
		startedAt := time.UnixMilli(m.StartedAtMS)
		s.appendRun(&CronRun{
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
			ErrorClass: ErrClassInterrupted,
			ErrorMsg:   "naozhi restarted while this run was in flight",
		})
		recovered++
	}
	if recovered > 0 {
		slog.Info("cron: recorded interrupted runs from the previous process",
			"count", recovered, "dir", dir)
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
