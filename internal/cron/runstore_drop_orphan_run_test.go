package cron

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runlog"
)

// newDropOrphanStore returns an enabled runStore rooted under a fresh
// TempDir plus a helper that appends a minimal run for jobID/runID.
func newDropOrphanStore(t *testing.T) (*runStore, func(jobID, runID string)) {
	t.Helper()
	tmp := t.TempDir()
	s := newRunStore(filepath.Join(tmp, "cron_jobs.json"), 10, time.Hour)
	if s == nil || !s.layout.Enabled() {
		t.Fatalf("newRunStore must succeed; got disabled")
	}
	s.enableTrimGC = false
	appendRun := func(jobID, runID string) {
		t.Helper()
		s.Append(&CronRun{
			JobID:     jobID,
			RunID:     runID,
			State:     RunStateSucceeded,
			Trigger:   TriggerScheduled,
			StartedAt: time.Now(),
			EndedAt:   time.Now(),
		})
		if _, err := os.Stat(filepath.Join(s.rootDir(), jobID, runID+".json")); err != nil {
			t.Fatalf("Append did not land %s/%s: %v", jobID, runID, err)
		}
	}
	return s, appendRun
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s must not exist (stat err=%v)", path, err)
	}
}

// TestRunStore_DropOrphanRun_OnlyOwnFileWhenSiblingsExist pins the #2479
// safety property: dropOrphanRun deletes exactly the caller's <runID>.json
// and leaves the directory (and every other record in it) alone when the
// directory is not empty — the non-recursive os.Remove(dir) must fail with
// ENOTEMPTY and be swallowed, never escalate to RemoveAll.
func TestRunStore_DropOrphanRun_OnlyOwnFileWhenSiblingsExist(t *testing.T) {
	t.Parallel()
	s, appendRun := newDropOrphanStore(t)
	jobID := mustGenerateID()
	mine, other := mustGenerateRunID(), mustGenerateRunID()
	appendRun(jobID, other)
	appendRun(jobID, mine)

	s.dropOrphanRun(jobID, mine)

	dir := filepath.Join(s.rootDir(), jobID)
	mustNotExist(t, filepath.Join(dir, mine+".json"))
	if _, err := os.Stat(filepath.Join(dir, other+".json")); err != nil {
		t.Fatalf("sibling record must survive: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("non-empty runs dir must survive: fi=%v err=%v", fi, err)
	}
}

// TestRunStore_DropOrphanRun_RemovesDirWhenEmpty: when the caller's record
// was the only entry, both the file and runs/<jobID>/ are removed, so no
// empty orphan directory outlives the deleted job.
func TestRunStore_DropOrphanRun_RemovesDirWhenEmpty(t *testing.T) {
	t.Parallel()
	s, appendRun := newDropOrphanStore(t)
	jobID := mustGenerateID()
	runID := mustGenerateRunID()
	appendRun(jobID, runID)

	s.dropOrphanRun(jobID, runID)

	mustNotExist(t, filepath.Join(s.rootDir(), jobID))
	// runs/ root itself is untouched.
	if fi, err := os.Stat(s.rootDir()); err != nil || !fi.IsDir() {
		t.Fatalf("runs root must survive: fi=%v err=%v", fi, err)
	}
}

// TestRunStore_DropOrphanRun_ClearsDirEnsuredCache: after dropOrphanRun
// removed runs/<jobID>/, the jobDirEnsured fast-path marker must be gone,
// otherwise the next Append would skip MkdirAll and fail its write. A
// follow-up Append must therefore land on disk again, and the recent cache
// must not serve the dropped run.
func TestRunStore_DropOrphanRun_ClearsDirMarkerAndCache(t *testing.T) {
	t.Parallel()
	s, appendRun := newDropOrphanStore(t)
	jobID := mustGenerateID()
	first, second := mustGenerateRunID(), mustGenerateRunID()
	appendRun(jobID, first)
	jobDir := filepath.Join(s.rootDir(), jobID)
	if fi, err := os.Stat(jobDir); err != nil || !fi.IsDir() {
		t.Fatalf("precondition: Append must create the job runs dir: %v", err)
	}
	// Warm the recent cache so we can prove dropOrphanRun invalidates it.
	if got := s.Recent(jobID, 5); len(got) != 1 || got[0].RunID != first {
		t.Fatalf("precondition: Recent = %+v, want [%s]", got, first)
	}

	s.dropOrphanRun(jobID, first)

	// dropOrphanRun rmdir'd the emptied job dir, so the layout's ensured marker
	// must have been dropped with it — asserted at the tail, where a later
	// Append has to MkdirAll the dir back.
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("dropOrphanRun must rmdir the emptied job dir: %v", err)
	}
	if _, ok := s.recentCache.Load(jobID); ok {
		t.Fatal("dropOrphanRun must invalidate the recent cache entry")
	}
	if got := s.Recent(jobID, 5); len(got) != 0 {
		t.Fatalf("Recent after drop = %+v, want empty", got)
	}

	// The directory is gone; a fresh Append must MkdirAll it back and succeed.
	// This is the observable form of "the ensured marker was cleared": a stale
	// marker would skip the MkdirAll and the record would be lost.
	appendRun(jobID, second)
	if fi, err := os.Stat(jobDir); err != nil || !fi.IsDir() {
		t.Fatalf("a later Append must recreate the job dir (stale ensured marker?): %v", err)
	}
	if got := s.Recent(jobID, 5); len(got) != 1 || got[0].RunID != second {
		t.Fatalf("Recent after re-append = %+v, want [%s]", got, second)
	}
}

// TestRunStore_DropOrphanRun_NoopOnMissingAndInvalid: missing file/dir and
// invalid IDs are silent no-ops (no panic, nothing created).
func TestRunStore_DropOrphanRun_NoopOnMissingAndInvalid(t *testing.T) {
	t.Parallel()
	s, _ := newDropOrphanStore(t)
	jobID := mustGenerateID()

	s.dropOrphanRun(jobID, mustGenerateRunID()) // nothing on disk
	mustNotExist(t, filepath.Join(s.rootDir(), jobID))

	s.dropOrphanRun("../escape", "0123456789abcdef")
	s.dropOrphanRun(jobID, "../escape")
	mustNotExist(t, filepath.Join(s.rootDir(), "../escape"))

	var nilStore *runStore
	nilStore.dropOrphanRun(jobID, "0123456789abcdef")
	(&runStore{layout: runlog.New(runlog.Options{})}).dropOrphanRun(jobID, "0123456789abcdef")
}
