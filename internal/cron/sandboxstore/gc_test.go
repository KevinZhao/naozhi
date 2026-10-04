package sandboxstore

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// gcFixture builds a store with a real snapshot tree: two jobs, three
// blobs — one referenced by a live manifest, one whose only manifest was
// trimmed (stranded), one brand-new and unreferenced (the in-flight window).
func gcFixture(t *testing.T) (st Store, root string, liveHash, strandedHash, freshHash string) {
	t.Helper()
	st = newTestStore(t)
	root = st.snapshotDir()
	if root == "" {
		t.Fatal("snapshot dir unresolved")
	}
	jobA, jobB := hexID(t), hexID(t)
	runA, runB := hexID(t), hexID(t)

	// Live: manifest + blob, both old.
	st.WriteSnapshot(jobA, runA, "live prompt content", "m", "img", nil, slog.Default())
	manA, ok, err := st.SnapshotManifest(jobA, runA)
	if err != nil || !ok {
		t.Fatalf("live manifest: %v %v", ok, err)
	}
	liveHash = manA.PromptHash

	// Stranded: write a full snapshot, then trim its manifest (what retention
	// does), leaving the blob with zero references.
	st.WriteSnapshot(jobB, runB, "stranded prompt content", "m", "img", nil, slog.Default())
	manB, _, _ := st.SnapshotManifest(jobB, runB)
	strandedHash = manB.PromptHash
	if err := os.Remove(filepath.Join(root, jobB, runB+".json")); err != nil {
		t.Fatal(err)
	}

	// Fresh unreferenced: the WriteSnapshot window — blob written,
	// manifest not yet landed.
	h, err := st.writeBlob(root, "fresh in-flight content")
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
	return st, root, liveHash, strandedHash, freshHash
}

// TestBlobGC_SweepsStrandedKeepsLiveAndFresh is the whole contract in one
// pass: the stranded blob (its only manifest trimmed) goes; the blob a live
// manifest references stays even though it is old — the direction easiest to
// get wrong, since age is exactly the wrong signal for content-addressed
// shared data; and the fresh unreferenced blob stays because it may belong to
// a manifest that has not landed yet (the WriteSnapshot window).
func TestBlobGC_SweepsStrandedKeepsLiveAndFresh(t *testing.T) {
	t.Parallel()
	st, root, liveHash, strandedHash, freshHash := gcFixture(t)

	st.GCBlobs()

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
	st, root, _, _, freshHash := gcFixture(t)
	old := time.Now().Add(-2 * blobGCGrace)
	if err := os.Chtimes(filepath.Join(root, "blobs", freshHash), old, old); err != nil {
		t.Fatal(err)
	}

	st.GCBlobs()

	if _, err := os.Stat(filepath.Join(root, "blobs", freshHash)); !os.IsNotExist(err) {
		t.Errorf("aged unreferenced blob survived (err=%v); the grace window must not be immortality", err)
	}
}

// TestBlobGC_NoSnapshotTreeIsNoop: a store without a snapshot tree (this
// host's normal state — sandbox placement unused) must do nothing, quietly.
func TestBlobGC_NoSnapshotTreeIsNoop(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	st.GCBlobs() // must not panic, must not create directories
	if entries, _ := os.ReadDir(st.Root); len(entries) != 0 {
		t.Errorf("GC on an empty root created %d entries", len(entries))
	}
}

// TestBlobGC_CollectsCrashedWriterTmpFiles: a writer that dies between
// CreateTemp and rename leaves <hash>.tmp-<rand> in the blob dir. Its name
// never equals a bare hash, so no manifest can reference it; past the grace
// it goes like any other unreferenced entry.
func TestBlobGC_CollectsCrashedWriterTmpFiles(t *testing.T) {
	t.Parallel()
	st, root, _, _, _ := gcFixture(t)
	tmp := filepath.Join(root, "blobs", "deadbeef.tmp-123456")
	if err := os.WriteFile(tmp, []byte("orphaned partial write"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * blobGCGrace)
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}

	st.GCBlobs()

	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("crashed-writer tmp file survived GC (err=%v)", err)
	}
}

// agedSnapshot writes a snapshot for prompt, trims its manifest (retention)
// and ages the blob past the grace: the blob is now old and unreferenced, the
// state in which a later run reusing the prompt dedups onto it.
func agedSnapshot(t *testing.T, st Store, jobID, prompt string) (blobPath string) {
	t.Helper()
	runID := hexID(t)
	st.WriteSnapshot(jobID, runID, prompt, "m", "img", nil, slog.Default())
	man, ok, err := st.SnapshotManifest(jobID, runID)
	if err != nil || !ok {
		t.Fatalf("manifest: %v %v", ok, err)
	}
	root := st.snapshotDir()
	if err := os.Remove(filepath.Join(root, jobID, runID+".json")); err != nil {
		t.Fatal(err)
	}
	blobPath = filepath.Join(root, "blobs", man.PromptHash)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(blobPath, old, old); err != nil {
		t.Fatal(err)
	}
	return blobPath
}

// assertReplayable fails unless runID's manifest resolves to prompt.
func assertReplayable(t *testing.T, st Store, jobID, runID, prompt string) {
	t.Helper()
	man, ok, err := st.SnapshotManifest(jobID, runID)
	if err != nil || !ok {
		t.Fatalf("manifest of the reusing run: ok=%v err=%v", ok, err)
	}
	if got, err := st.SnapshotPrompt(man.PromptHash); err != nil || got != prompt {
		t.Errorf("SnapshotPrompt = %q, %v; want the run's prompt — its manifest dangles", got, err)
	}
}

// TestWriteBlob_DedupHitRefreshesMtime: reusing an old blob makes it young
// again, which is what keeps it out of a GC sweep whose mark predates the
// reusing run's manifest.
func TestWriteBlob_DedupHitRefreshesMtime(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	blob := agedSnapshot(t, st, hexID(t), "shared prompt")

	if _, err := st.writeBlob(st.snapshotDir(), "shared prompt"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(fi.ModTime()); age > blobGCGrace {
		t.Errorf("deduped blob is %v old after reuse; the GC age barrier would not protect it", age)
	}
}

// TestWriteBlob_RewritesWhenBlobVanished: a dedup check that cannot touch the
// blob (the GC removed it) must write it again rather than trust it.
func TestWriteBlob_RewritesWhenBlobVanished(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	root := st.snapshotDir()
	hash, err := st.writeBlob(root, "vanishing prompt")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "blobs", hash)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := st.writeBlob(root, "vanishing prompt"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "vanishing prompt" {
		t.Errorf("blob after rewrite = %q, %v", b, err)
	}
}

// TestBlobGC_DedupHitAfterMarkSurvives is the issue's sequence: the mark sees
// an old blob with no references, then a run reuses its prompt and lands a
// manifest before the sweep. The sweep must not delete the blob under it.
func TestBlobGC_DedupHitAfterMarkSurvives(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	job, run2 := hexID(t), hexID(t)
	blob := agedSnapshot(t, st, job, "shared prompt")

	st.gcBlobs(gcHooks{afterMark: func() {
		st.WriteSnapshot(job, run2, "shared prompt", "m", "img", nil, slog.Default())
	}})

	if _, err := os.Stat(blob); err != nil {
		t.Errorf("reused blob swept: %v", err)
	}
	assertReplayable(t, st, job, run2, "shared prompt")
}

// TestBlobGC_SweepExcludesWriters: a run reusing the prompt between the
// sweep's age check and its remove must not end up with a dangling manifest.
// The writer is started at exactly that point and given a moment to finish;
// with the sweep holding blobMu it cannot, and runs once the remove is done,
// finding the blob gone and rewriting it.
func TestBlobGC_SweepExcludesWriters(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	job, run2 := hexID(t), hexID(t)
	blob := agedSnapshot(t, st, job, "shared prompt")

	done := make(chan struct{})
	st.gcBlobs(gcHooks{beforeRemove: func(string) {
		go func() {
			defer close(done)
			st.WriteSnapshot(job, run2, "shared prompt", "m", "img", nil, slog.Default())
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}})
	<-done

	if _, err := os.Stat(blob); err != nil {
		t.Errorf("blob missing after a writer raced the sweep: %v", err)
	}
	assertReplayable(t, st, job, run2, "shared prompt")
}

// TestBlobGC_ConcurrentWritersNeverDangle runs writers that keep reusing a
// few prompts against repeated GC passes, with retention trimming manifests
// and the blobs aged between passes. Aging holds blobMu exclusively: time
// passing between writes, not inside one (that stall is what blobGCGrace
// covers). Every manifest left on disk must still resolve to its prompt.
func TestBlobGC_ConcurrentWritersNeverDangle(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	root := st.snapshotDir()
	job := hexID(t)
	prompts := []string{"prompt one", "prompt two", "prompt three"}
	for _, p := range prompts {
		agedSnapshot(t, st, job, p)
	}

	const writers, perWriter, passes = 3, 20, 15
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		runIDs := make([]string, perWriter)
		for i := range runIDs {
			runIDs[i] = hexID(t)
		}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i, runID := range runIDs {
				st.WriteSnapshot(job, runID, prompts[(w+i)%len(prompts)], "m", "img", nil, slog.Default())
			}
		}(w)
	}
	old := time.Now().Add(-48 * time.Hour)
	for pass := 0; pass < passes; pass++ {
		manifests, _ := os.ReadDir(filepath.Join(root, job))
		for i, mf := range manifests {
			if i%2 == 0 {
				_ = os.Remove(filepath.Join(root, job, mf.Name()))
			}
		}
		blobMu.Lock()
		blobs, _ := os.ReadDir(filepath.Join(root, "blobs"))
		for _, b := range blobs {
			_ = os.Chtimes(filepath.Join(root, "blobs", b.Name()), old, old)
		}
		blobMu.Unlock()
		st.GCBlobs()
	}
	wg.Wait()

	manifests, err := os.ReadDir(filepath.Join(root, job))
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range manifests {
		man, ok, err := readManifest(filepath.Join(root, job, mf.Name()))
		if err != nil || !ok {
			t.Fatalf("manifest %s: ok=%v err=%v", mf.Name(), ok, err)
		}
		if got, err := st.SnapshotPrompt(man.PromptHash); err != nil || got == "" {
			t.Errorf("manifest %s dangles: SnapshotPrompt = %q, %v", mf.Name(), got, err)
		}
	}
}
