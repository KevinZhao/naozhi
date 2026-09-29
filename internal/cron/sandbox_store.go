package cron

import (
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
