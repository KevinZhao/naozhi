package cron

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sandboxAttention is the §7.4 confirmation-queue record. A sandbox run that
// ends in an UNKNOWN-fate state writes one of these; it stays on disk until an
// operator resolves it (confirm-done or replay), which is the only human step
// in the §6.2 double-run containment.
//
// Two producers write it:
//
//   - executeSandbox's failed-transport branch, but ONLY when the job declared
//     side_effects=true (RFC §6.2 rule 3: a side-effecting job must not
//     auto-replay; it waits for a human to check whether the side effect
//     already happened). A side-effect-free transport failure is safe to just
//     re-run and never enters the queue.
//   - reconcileOneSandboxOrphan (§6.5): a run orphaned by a naozhi restart.
//     Orphans of side-effecting jobs enter the queue too (the microVM may have
//     completed and pushed a PR while naozhi was down).
//
// Stored at <store-dir>/sandboxattention/<runID>.json (flat, like
// sandboxpending) — the queue is small (operator-actioned) and a flat dir
// scans fast. RunID is scheduler-generated hex, path-safe by construction.
type sandboxAttention struct {
	JobID string `json:"job_id"`
	RunID string `json:"run_id"`
	// RuntimeSessionID is the platform session id of the orphaned/lost run —
	// the handle ReplaySandboxRun needs to satisfy §6.2 rule 1 (StopSession
	// before any replay). Empty only for records written without a known
	// session (defensive; replay then treats the fate as already-terminal).
	RuntimeSessionID string `json:"runtime_session_id,omitempty"`
	// Reason classifies why the run needs attention, surfaced in the queue
	// card. One of attentionReason* below.
	Reason string `json:"reason"`
	// JobLabel is the human title at write time, so the queue card renders a
	// name even after the job is edited/deleted. SanitizeForLog'd at the
	// dashboard edge, not here (cron stores the raw snapshot value).
	JobLabel string `json:"job_label,omitempty"`
	// StartedAtMS is the original run's start (unix-ms) for the card timestamp.
	StartedAtMS int64 `json:"started_at_ms"`
	// CreatedAtMS is when the record entered the queue (unix-ms).
	CreatedAtMS int64 `json:"created_at_ms"`
}

const (
	// attentionReasonTransport: stream lost mid-run, microVM fate unknown,
	// job declares side effects (RFC §6.2 rule 3).
	attentionReasonTransport = "transport"
	// attentionReasonOrphaned: naozhi restarted while the run was in flight
	// (RFC §6.5); the orphan reconcile Stopped the microVM but a side effect
	// may already have landed.
	attentionReasonOrphaned = "orphaned"
)

// sandboxAttentionDir resolves the queue directory ("" when persistence is
// disabled — store-less test fixtures skip the queue entirely).
func (s *Scheduler) sandboxAttentionDir() string {
	if s.storePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.storePath), "sandboxattention")
}

// writeSandboxAttention persists one queue record. Best-effort: a write
// failure logs and returns (the run's terminal record is already durable; the
// queue entry is an operator convenience, not a correctness invariant). The
// run's failed-transport CronRun still warns "check for side effects", so a
// missed queue entry degrades to "operator reads run history" not "silent
// double-run" — the §6.2 safety is in the no-auto-replay rule, not the queue.
func (s *Scheduler) writeSandboxAttention(rec sandboxAttention, lg *slog.Logger) {
	dir := s.sandboxAttentionDir()
	if dir == "" {
		return
	}
	if !IsValidID(rec.JobID) || !IsValidID(rec.RunID) {
		lg.Warn("cron sandbox: attention write rejected non-hex id", "job_id", rec.JobID, "run_id", rec.RunID)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		lg.Warn("cron sandbox: attention dir create failed; run not enqueued for confirmation", "err", err)
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		lg.Warn("cron sandbox: attention marshal failed", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, rec.RunID+".json"), b, 0o600); err != nil {
		lg.Warn("cron sandbox: attention write failed; run not enqueued for confirmation", "err", err)
	}
}

// removeSandboxAttention deletes a resolved queue record. Idempotent: a
// missing file (already resolved by a concurrent action) is not an error.
func (s *Scheduler) removeSandboxAttention(runID string) error {
	dir := s.sandboxAttentionDir()
	if dir == "" {
		return nil
	}
	if !IsValidID(runID) {
		return errInvalidAttentionID
	}
	err := os.Remove(filepath.Join(dir, runID+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// getSandboxAttention reads one queue record (the replay/confirm path needs
// the runtime session id + jobID). (nil, false, nil) when the record does not
// exist — already resolved, or never enqueued.
func (s *Scheduler) getSandboxAttention(runID string) (*sandboxAttention, bool, error) {
	dir := s.sandboxAttentionDir()
	if dir == "" {
		return nil, false, nil
	}
	if !IsValidID(runID) {
		return nil, false, errInvalidAttentionID
	}
	raw, err := os.ReadFile(filepath.Join(dir, runID+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var rec sandboxAttention
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, false, err
	}
	return &rec, true, nil
}

// SandboxAttentionItem is the read-model returned to the dashboard queue
// (RFC §7.4). It re-exports the on-disk record's safe fields; the runtime
// session id is deliberately NOT exposed (operator-irrelevant, and it is an
// internal platform handle).
type SandboxAttentionItem struct {
	JobID       string `json:"job_id"`
	RunID       string `json:"run_id"`
	Reason      string `json:"reason"`
	JobLabel    string `json:"job_label,omitempty"`
	StartedAtMS int64  `json:"started_at_ms,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms,omitempty"`
}

// ListSandboxAttention returns every unresolved §7.4 queue record, newest
// first (by CreatedAtMS). Corrupt records are skipped (logged once) rather
// than failing the whole list — one bad file must not hide the rest of the
// queue. Returns an empty slice (never nil) so the dashboard renders an empty
// queue consistently.
func (s *Scheduler) ListSandboxAttention() []SandboxAttentionItem {
	out := []SandboxAttentionItem{}
	dir := s.sandboxAttentionDir()
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cron sandbox: attention scan failed", "err", err)
		}
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // benign: a concurrent resolve may have removed it
		}
		var rec sandboxAttention
		if err := json.Unmarshal(raw, &rec); err != nil || !IsValidID(rec.RunID) || !IsValidID(rec.JobID) {
			slog.Warn("cron sandbox: corrupt attention record skipped", "file", e.Name())
			continue
		}
		out = append(out, SandboxAttentionItem{
			JobID:       rec.JobID,
			RunID:       rec.RunID,
			Reason:      rec.Reason,
			JobLabel:    rec.JobLabel,
			StartedAtMS: rec.StartedAtMS,
			CreatedAtMS: rec.CreatedAtMS,
		})
	}
	// Newest first: the queue reads as a stack of recent incidents.
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAtMS > out[j].CreatedAtMS
	})
	return out
}

// SandboxAttentionCount returns the number of unresolved queue records — the
// header cron-badge attention counter (RFC §7.2 "把 failed-transport 与
// orphaned 计入待处理"). Cheap dir-entry count; skips non-.json noise.
func (s *Scheduler) SandboxAttentionCount() int {
	dir := s.sandboxAttentionDir()
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// deleteJobAttention removes every queue record belonging to jobID (called
// from deleteJobRuns when a job is deleted, §7.4). Records are keyed by runID
// in a flat dir, so this scans and matches on the JobID field. Best-effort:
// a read/remove failure on one record does not abort the rest.
func (s *Scheduler) deleteJobAttention(jobID string) {
	dir := s.sandboxAttentionDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var rec sandboxAttention
		if json.Unmarshal(raw, &rec) != nil || rec.JobID != jobID {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("cron sandbox: delete-job attention remove failed", "job_id", jobID, "file", e.Name(), "err", err)
		}
	}
}

// attentionNowMS is the injectable clock read for the queue record's
// CreatedAtMS. Uses the scheduler clock so tests pin a deterministic value.
func (s *Scheduler) attentionNowMS() int64 {
	return s.now().UnixMilli()
}

// WriteSandboxAttentionForTest is an exported seam so consumer-package tests
// (dashboard handlers) can stage a §7.4 queue record without driving a full
// failed-transport run. Production code uses the unexported
// writeSandboxAttention. NOT for runtime use.
func (s *Scheduler) WriteSandboxAttentionForTest(jobID, runID, reason, jobLabel string) {
	s.writeSandboxAttention(sandboxAttention{
		JobID:       jobID,
		RunID:       runID,
		Reason:      reason,
		JobLabel:    jobLabel,
		CreatedAtMS: s.attentionNowMS(),
	}, slog.Default())
}
