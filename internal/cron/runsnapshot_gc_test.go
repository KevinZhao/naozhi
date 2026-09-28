package cron

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScheduler_Start_RunsBlobGC pins the wiring: Start alone (no manual
// GCBlobs call) must collect the stranded blob. This is the test that
// goes red if the startup pass is dropped or re-gated behind a store that is
// not the one snapshots live in.
func TestScheduler_Start_RunsBlobGC(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(t.TempDir(), "cron_jobs.json")}, SchedulerDeps{Router: &fakeRouter{}})
	root := s.stateSubtree("runsnapshots")
	jobA, jobB := mustGenerateID(), mustGenerateID()
	runA, runB := mustGenerateRunID(), mustGenerateRunID()
	// Live: an old blob a manifest still references. Stranded: an old blob
	// whose only manifest was trimmed.
	s.sandboxState().WriteSnapshot(jobA, runA, "live prompt content", "m", "img", nil, slog.Default())
	s.sandboxState().WriteSnapshot(jobB, runB, "stranded prompt content", "m", "img", nil, slog.Default())
	manA, _, _ := s.SandboxRunSnapshotManifest(jobA, runA)
	manB, _, _ := s.SandboxRunSnapshotManifest(jobB, runB)
	liveHash, strandedHash := manA.PromptHash, manB.PromptHash
	if err := os.Remove(filepath.Join(root, jobB, runB+".json")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, hash := range []string{liveHash, strandedHash} {
		if err := os.Chtimes(filepath.Join(root, "blobs", hash), old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	s.gcWG.Wait()

	if _, err := os.Stat(filepath.Join(root, "blobs", strandedHash)); !os.IsNotExist(err) {
		t.Errorf("stranded blob survived Start (err=%v); blob GC is not wired into the startup passes", err)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs", liveHash)); err != nil {
		t.Errorf("live blob deleted by Start: %v", err)
	}
}
