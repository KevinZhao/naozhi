package sandboxstore

// gc.go — mark-sweep for the content-addressed blob store
// (#2682, agentcore §5.2's TODO).
//
// Blobs are deduped across jobs and runs, so retention can never age them
// out the way it ages manifests: a months-old blob may be referenced by a
// manifest written this morning — that is the point of dedup. When the LAST
// manifest referencing a blob is trimmed, though, nothing ever deleted the
// blob; the tree only grew. The pass below marks every hash a live manifest
// references, then sweeps the rest.
//
// The hard part is the window WriteSnapshot opens: it writes the blob
// FIRST, then the manifest (a truncated manifest must never dangle a hash to
// a missing blob — the same reason readers get atomic writes). A mark taken
// before the manifest lands misses the blob, so the mark set is stale by the
// time the sweep runs. Two rules make that harmless. Every blob write,
// including a dedup hit on an old blob, leaves the blob younger than
// blobGCGrace, and the sweep spares young blobs. And blobMu orders the two
// sides: WriteSnapshot holds it shared from the blob write to the manifest
// write, and the sweep holds it exclusively for each candidate's fresh lstat
// and remove, so a remove lands either before a writer's touch (the writer
// sees the blob gone and rewrites it) or after its manifest (the lstat sees
// the touch). Writers only share the lock, so runs never wait on each other.

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// blobMu orders snapshot writes against the blob sweep; see the file header.
// Package-level because every writer of a snapshot tree is in-process and
// Store is a by-value handle built ad hoc.
var blobMu sync.RWMutex

// blobGCGrace is how young a blob must be for the sweep to spare it
// unconditionally. It only needs to out-span the blob→manifest write gap
// (microseconds); an hour also covers pathological stalls (a wedged disk, a
// paused VM) with six orders of magnitude to spare. Var for tests.
var blobGCGrace = time.Hour

// GCBlobs removes blobs no live manifest references. cron runs it as a startup
// pass next to the run-history GC: retention trims manifests, and a trimmed
// manifest is exactly what strands a blob.
func (st Store) GCBlobs() { st.gcBlobs(gcHooks{}) }

// gcHooks are test seams into one GC pass; production passes the zero value.
type gcHooks struct {
	afterMark    func()            // between the mark and the sweep
	beforeRemove func(name string) // under blobMu, after the age re-check
}

func (st Store) gcBlobs(h gcHooks) {
	root := st.snapshotDir()
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
			man, ok, err := readManifest(filepath.Join(root, jd.Name(), mf.Name()))
			if err != nil || !ok {
				continue
			}
			if man.PromptHash != "" {
				live[man.PromptHash] = struct{}{}
			}
		}
	}

	if h.afterMark != nil {
		h.afterMark()
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
		if sweepBlob(blobDir, name, cutoff, h.beforeRemove) {
			removed++
		}
	}
	if removed > 0 {
		slog.Info("cron sandbox: blob GC removed unreferenced blobs",
			"removed", removed, "live", len(live), "scanned", len(blobs))
	}
}

// sweepBlob removes one unmarked blob if it is still older than cutoff. The
// age is re-read under blobMu rather than taken from the directory listing: a
// writer may have touched the blob since the mark (a dedup hit), and only a
// check made while writers are excluded can trust what it sees. Leftover
// .tmp-* files from crashed writers age past the same cutoff and go too.
func sweepBlob(blobDir, name string, cutoff time.Time, beforeRemove func(string)) bool {
	path := filepath.Join(blobDir, name)
	blobMu.Lock()
	defer blobMu.Unlock()
	info, err := os.Lstat(path)
	if err != nil || info.ModTime().After(cutoff) {
		return false
	}
	if beforeRemove != nil {
		beforeRemove(name)
	}
	if err := os.Remove(path); err != nil {
		slog.Warn("cron sandbox: blob GC remove failed", "blob", name, "err", err)
		return false
	}
	return true
}
