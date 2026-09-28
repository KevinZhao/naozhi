package cron

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
	"github.com/naozhi/naozhi/internal/datadir"
)

// The sandbox state on disk — confirmation queue, run-input snapshots, event
// logs — is read and written by sandboxstore. This file is the Scheduler's
// handle on it plus the exported API the dashboard consumes.

// SandboxAttentionItem is one §7.4 confirmation-queue entry as the dashboard
// renders it.
type SandboxAttentionItem = sandboxstore.AttentionItem

// SandboxRunSnapshot is the input manifest of one sandbox run.
type SandboxRunSnapshot = sandboxstore.RunSnapshot

// ErrSandboxEventsBusy is returned by SandboxRunEvents when the process-wide
// read gate is saturated. The dashboard maps it to HTTP 503.
var ErrSandboxEventsBusy = sandboxstore.ErrEventsBusy

// sandboxState returns the store rooted at the cron state directory (the zero Store,
// persistence disabled, when the scheduler has no store path).
func (s *Scheduler) sandboxState() sandboxstore.Store {
	return sandboxstore.Store{Root: datadir.ForStore(s.storePath).Root()}
}

// ListSandboxAttention returns every unresolved queue record, newest first.
func (s *Scheduler) ListSandboxAttention() []SandboxAttentionItem {
	return s.sandboxState().ListAttention()
}

// SandboxAttentionCount returns the number of queue records on disk.
func (s *Scheduler) SandboxAttentionCount() int {
	return s.sandboxState().AttentionCount()
}

// SandboxRunSnapshotManifest reads one run's input manifest; (nil, false, nil)
// when there is none.
func (s *Scheduler) SandboxRunSnapshotManifest(jobID, runID string) (*SandboxRunSnapshot, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	return s.sandboxState().SnapshotManifest(jobID, runID)
}

// SandboxRunSnapshotPrompt reads the prompt blob a manifest references.
func (s *Scheduler) SandboxRunSnapshotPrompt(blobHash string) (string, error) {
	if s == nil {
		return "", nil
	}
	return s.sandboxState().SnapshotPrompt(blobHash)
}

// SandboxRunEvents reads up to maxLines lines of one sandbox run's event log.
func (s *Scheduler) SandboxRunEvents(jobID, runID string, maxLines int) ([][]byte, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	return s.sandboxState().RunEvents(jobID, runID, maxLines)
}

// attentionNowMS is the injectable clock read for the queue record's
// CreatedAtMS. Uses the scheduler clock so tests pin a deterministic value.
func (s *Scheduler) attentionNowMS() int64 {
	return s.now().UnixMilli()
}

// WriteSandboxAttentionForTest is an exported seam so consumer-package tests
// (dashboard handlers) can stage a §7.4 queue record without driving a full
// failed-transport run. NOT for runtime use.
func (s *Scheduler) WriteSandboxAttentionForTest(jobID, runID, reason, jobLabel string) {
	s.sandboxState().WriteAttention(sandboxstore.Attention{
		JobID:       jobID,
		RunID:       runID,
		Reason:      reason,
		JobLabel:    jobLabel,
		CreatedAtMS: s.attentionNowMS(),
	}, slog.Default())
}

// WriteSandboxSnapshotForTest is an exported seam so consumer-package tests
// (dashboard handlers) can stage a snapshot without driving a full run. NOT
// for runtime use — mirrors agentcore.NewWithAPIForTest.
func (s *Scheduler) WriteSandboxSnapshotForTest(jobID, runID, prompt, model, imageVersion string, secretRefs []string) {
	s.sandboxState().WriteSnapshot(jobID, runID, prompt, model, imageVersion, secretRefs, slog.Default())
}
