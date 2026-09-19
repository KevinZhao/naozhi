package cron

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gcFixture builds a scheduler with a real snapshot tree: two jobs, three
// blobs — one referenced by a live manifest, one whose only manifest was
// trimmed (stranded), one brand-new and unreferenced (the in-flight window).
func gcFixture(t *testing.T) (s *Scheduler, root string, liveHash, strandedHash, freshHash string) {
	t.Helper()
	dir := t.TempDir()
	s = NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(dir, "cron_jobs.json")}, SchedulerDeps{Router: &fakeRouter{}})
	root = s.sandboxSnapshotDir()
	if root == "" {
		t.Fatal("snapshot dir unresolved")
	}
	jobA, jobB := mustGenerateID(), mustGenerateID()
	runA, runB := mustGenerateRunID(), mustGenerateRunID()

	// Live: manifest + blob, both old.
	s.writeSandboxSnapshot(jobA, runA, "live prompt content", "m", "img", nil, slog.Default())
	manA, ok, err := s.SandboxRunSnapshotManifest(jobA, runA)
	if err != nil || !ok {
		t.Fatalf("live manifest: %v %v", ok, err)
	}
	liveHash = manA.PromptHash

	// Stranded: write a full snapshot, then trim its manifest (what retention
	// does), leaving the blob with zero references.
	s.writeSandboxSnapshot(jobB, runB, "stranded prompt content", "m", "img", nil, slog.Default())
	manB, _, _ := s.SandboxRunSnapshotManifest(jobB, runB)
	strandedHash = manB.PromptHash
	if err := os.Remove(filepath.Join(root, jobB, runB+".json")); err != nil {
		t.Fatal(err)
	}

	// Fresh unreferenced: the writeSandboxSnapshot window — blob written,
	// manifest not yet landed.
	h, err := s.writeSnapshotBlob(root, "fresh in-flight content")
	if err != nil {
		t.Fatal(err)
	}
	freshHash = h

	// Age the live and stranded blobs (and the live manifest) past any grace.
	old := time.Now().Add(-48 * time.Hour)
	for _, hash := range []string{liveHash, strandedHash} {
		if err := os.Chtimes(filepath.Join(root, "blobs", hash), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return s, root, liveHash, strandedHash, freshHash
}

// TestBlobGC_SweepsStrandedKeepsLiveAndFresh is the whole contract in one
// pass: the stranded blob (its only manifest trimmed) goes; the blob a live
// manifest references stays even though it is old — the direction easiest to
// get wrong, since age is exactly the wrong signal for content-addressed
// shared data; and the fresh unreferenced blob stays because it may belong to
// a manifest that has not landed yet (the writeSandboxSnapshot window).
func TestBlobGC_SweepsStrandedKeepsLiveAndFresh(t *testing.T) {
	t.Parallel()
	s, root, liveHash, strandedHash, freshHash := gcFixture(t)

	s.gcSandboxBlobs()

	blobPath := func(h string) string { return filepath.Join(root, "blobs", h) }
	if _, err := os.Stat(blobPath(strandedHash)); !os.IsNotExist(err) {
		t.Errorf("stranded blob survived GC (err=%v); the tree only ever grows again", err)
	}
	if _, err := os.Stat(blobPath(liveHash)); err != nil {
		t.Errorf("LIVE blob deleted: %v — an old blob referenced by a live manifest is the dedup case, not garbage", err)
	}
	if _, err := os.Stat(blobPath(freshHash)); err != nil {
		t.Errorf("fresh unreferenced blob deleted: %v — its manifest may be mid-write (the blob-first ordering)", err)
	}
}

// TestBlobGC_FreshBlobBecomesSweepableOncePastGrace: the same unreferenced
// blob, aged past the grace with still no manifest, is garbage after all —
// the window protection must not become immortality.
func TestBlobGC_FreshBlobBecomesSweepableOncePastGrace(t *testing.T) {
	t.Parallel()
	s, root, _, _, freshHash := gcFixture(t)
	old := time.Now().Add(-2 * blobGCGrace)
	if err := os.Chtimes(filepath.Join(root, "blobs", freshHash), old, old); err != nil {
		t.Fatal(err)
	}

	s.gcSandboxBlobs()

	if _, err := os.Stat(filepath.Join(root, "blobs", freshHash)); !os.IsNotExist(err) {
		t.Errorf("aged unreferenced blob survived (err=%v); the grace window must not be immortality", err)
	}
}

// TestBlobGC_NoStoreIsNoop: a scheduler without a snapshot tree (this host's
// normal state — sandbox placement unused) must do nothing, quietly.
func TestBlobGC_NoStoreIsNoop(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}})
	s.gcSandboxBlobs() // must not panic, must not create directories
}

// TestBlobGC_CollectsCrashedWriterTmpFiles: a writer that dies between
// CreateTemp and rename leaves <hash>.tmp-<rand> in the blob dir. Its name
// never equals a bare hash, so no manifest can reference it; past the grace
// it goes like any other unreferenced entry.
func TestBlobGC_CollectsCrashedWriterTmpFiles(t *testing.T) {
	t.Parallel()
	s, root, _, _, _ := gcFixture(t)
	tmp := filepath.Join(root, "blobs", "deadbeef.tmp-123456")
	if err := os.WriteFile(tmp, []byte("orphaned partial write"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * blobGCGrace)
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}

	s.gcSandboxBlobs()

	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("crashed-writer tmp file survived GC (err=%v)", err)
	}
}

// TestScheduler_Start_RunsBlobGC pins the wiring: Start alone (no manual
// gcSandboxBlobs call) must collect the stranded blob. This is the test that
// goes red if the startup pass is dropped or re-gated behind a store that is
// not the one snapshots live in.
func TestScheduler_Start_RunsBlobGC(t *testing.T) {
	t.Parallel()
	s, root, liveHash, strandedHash, _ := gcFixture(t)
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
