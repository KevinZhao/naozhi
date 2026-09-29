package cron

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
)

// TestSandboxStore_IDShapeMatchesCron: sandboxstore validates ids itself (it
// cannot import cron), and its answer must be IsValidID's. A drift either way
// is a bug: looser lets a path the scheduler never generates reach the disk,
// stricter refuses records the scheduler wrote.
func TestSandboxStore_IDShapeMatchesCron(t *testing.T) {
	t.Parallel()
	st := sandboxstore.Store{Root: t.TempDir()}
	ids := []string{
		"", "0", "f", "0123456789abcdef", mustGenerateID(), mustGenerateRunID(),
		strings.Repeat("a", 64), strings.Repeat("a", 65),
		"ABCDEF", "g", "0123456789abcdeF", "../etc", "a/b", "a.b", "a b", "a\x00b", "é",
	}
	for _, id := range ids {
		err := st.RemoveAttention(id)
		if gotValid := !errors.Is(err, sandboxstore.ErrInvalidID); gotValid != IsValidID(id) {
			t.Errorf("id %q: store accepts=%v, IsValidID=%v (err %v)", id, gotValid, IsValidID(id), err)
		}
	}
}

// TestSandboxStore_RootIsTheCronStateDir: the Scheduler's store is rooted at
// the directory holding the cron store file, so what cron writes the store
// reads back from the same paths — and a store-less scheduler gets the zero
// Store.
func TestSandboxStore_RootIsTheCronStateDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(dir, "cron_jobs.json")}, SchedulerDeps{Router: &fakeRouter{}})
	if got := s.sandboxState().Root; got != dir {
		t.Fatalf("store root = %q, want the cron state dir %q", got, dir)
	}
	jobID, runID := mustGenerateID(), mustGenerateRunID()
	s.sandboxState().WriteAttention(sandboxstore.Attention{JobID: jobID, RunID: runID, Reason: sandboxstore.ReasonTransport, JobLabel: "nightly", CreatedAtMS: s.attentionNowMS()}, slog.Default())
	if _, err := os.Stat(filepath.Join(dir, "sandboxattention", runID+".json")); err != nil {
		t.Fatalf("attention record not at <state-dir>/sandboxattention: %v", err)
	}
	rec, ok, err := sandboxstore.Store{Root: dir}.GetAttention(runID)
	if err != nil || !ok || rec.JobID != jobID {
		t.Fatalf("a Store over the same dir reads (%+v, %v, %v)", rec, ok, err)
	}

	bare := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	if got := bare.sandboxState(); got != (sandboxstore.Store{}) {
		t.Fatalf("store-less scheduler store = %+v, want the zero Store", got)
	}
	bare.sandboxState().WriteSnapshot(jobID, runID, "p", "", "", nil, slog.Default())
	if man, ok, err := bare.SandboxRunSnapshotManifest(jobID, runID); man != nil || ok || err != nil {
		t.Fatalf("store-less manifest = (%v, %v, %v), want absent", man, ok, err)
	}
}
