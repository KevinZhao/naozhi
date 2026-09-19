package cron

// runsnapshot_gc.go — mark-sweep for the content-addressed blob store
// (#2682, agentcore §5.2's TODO).
//
// Blobs are deduped across jobs and runs, so retention can never age them
// out the way it ages manifests: a months-old blob may be referenced by a
// manifest written this morning — that is the point of dedup. When the LAST
// manifest referencing a blob is trimmed, though, nothing ever deleted the
// blob; the tree only grew. The pass below marks every hash a live manifest
// references, then sweeps the rest.
//
// The hard part is the window writeSandboxSnapshot opens: it writes the blob
// FIRST, then the manifest (a truncated manifest must never dangle a hash to
// a missing blob — the same reason readers get atomic writes). A mark taken
// between those two steps misses the new blob and the sweep would delete it.
// The barrier is age, not locks: the blob-to-manifest gap is microseconds
// inside one function, and the sweep refuses to touch any blob younger than
// blobGCGrace. A grace window costs nothing (GC is space reclamation, not
// timeliness) where a lock on the snapshot write path would tax every run.

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// blobGCGrace is how young a blob must be for the sweep to spare it
// unconditionally. It only needs to out-span the blob→manifest write gap
// (microseconds); an hour also covers pathological stalls (a wedged disk, a
// paused VM) with six orders of magnitude to spare. Var for tests.
var blobGCGrace = time.Hour

// gcSandboxBlobs removes blobs no live manifest references. Startup pass,
// scheduled next to the run-history GC: retention trims manifests, and a
// trimmed manifest is exactly what strands a blob.
func (s *Scheduler) gcSandboxBlobs() {
	root := s.sandboxSnapshotDir()
	if root == "" {
		return
	}
	blobDir := filepath.Join(root, "blobs")
	blobs, err := os.ReadDir(blobDir)
	if err != nil || len(blobs) == 0 {
		return // no store, nothing stranded
	}

	// Mark: every hash any readable manifest references. A corrupt manifest
	// marks nothing — it cannot name a blob, and its run is unreplayable
	// regardless, so blobs only it referenced are garbage like any other.
	live := make(map[string]struct{})
	jobDirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-blobGCGrace)
	for _, jd := range jobDirs {
		if !jd.IsDir() || jd.Name() == "blobs" {
			continue
		}
		manifests, err := os.ReadDir(filepath.Join(root, jd.Name()))
		if err != nil {
			continue
		}
		for _, mf := range manifests {
			if mf.IsDir() {
				continue
			}
			man, ok, err := readSandboxSnapshotManifest(filepath.Join(root, jd.Name(), mf.Name()))
			if err != nil || !ok {
				continue
			}
			if man.PromptHash != "" {
				live[man.PromptHash] = struct{}{}
			}
		}
	}

	removed := 0
	for _, b := range blobs {
		if b.IsDir() {
			continue
		}
		name := b.Name()
		if _, referenced := live[name]; referenced {
			continue
		}
		// The age barrier: a blob younger than the grace may belong to a
		// manifest that has not landed yet (writeSandboxSnapshot writes the
		// blob first). Leftover .tmp-* files from crashed writers age past the
		// same cutoff and get collected with everything else.
		info, err := b.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(blobDir, name)); err != nil {
			slog.Warn("cron sandbox: blob GC remove failed", "blob", name, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("cron sandbox: blob GC removed unreferenced blobs",
			"removed", removed, "live", len(live), "scanned", len(blobs))
	}
}
