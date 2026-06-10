package cron

// R20260527-GO-12 (#1301): persistOnShutdown ignored save()'s
// WriteFileAtomic failure because saveMarshaledSeq returns void; this
// test pins the indirect detection path that compares lastSavedSeq
// before vs after save() so a write failure produces a
// FAILED_DURING_SHUTDOWN-tagged log line instead of silent data loss.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPersistOnShutdown_DetectsWriteFailure forces saveMarshaledSeq's
// WriteFileAtomic call to fail by pre-creating storePath as a directory.
// Without the fix, persistOnShutdown returns successfully even though
// nothing landed; with the fix, lastSavedSeq Load() < queuedSeq triggers
// the operator-visible error log. We assert the in-memory state was
// not somehow persisted (the directory still exists, no file replaced
// it) so the staleness-on-restart symptom #1301 describes is reproduced.
func TestPersistOnShutdown_DetectsWriteFailure(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{StorePath: storePath, MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if err := s.AddJob(&Job{
		Schedule: "@every 1h",
		Prompt:   "p",
		Platform: "x",
		ChatID:   "c",
		Paused:   true,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Now corrupt storePath by replacing the just-written file with a
	// directory of the same name; the next WriteFileAtomic rename will
	// fail (EISDIR or similar) and saveMarshaledSeq leaves lastSavedSeq
	// pinned at the AddJob's success seq.
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("remove storePath: %v", err)
	}
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatalf("seed dir at storePath: %v", err)
	}

	// AddJob another to bump saveSeq beyond what's on disk. The mutation
	// itself succeeds in memory; the persist closure schedules a save
	// that the storePath-is-a-dir setup will fail.
	if err := s.AddJob(&Job{
		Schedule: "@every 2h",
		Prompt:   "p2",
		Platform: "x",
		ChatID:   "c2",
		Paused:   true,
	}); err != nil {
		// In-memory mutation always succeeds; the save error is async
		// and only logged. If this errors out, the contract changed.
		t.Fatalf("second AddJob: %v", err)
	}

	queuedBefore := s.saveSeq.Load()
	s.persistOnShutdown()
	queuedAfter := s.saveSeq.Load()
	if queuedAfter <= queuedBefore {
		t.Fatalf("saveSeq did not advance: before=%d after=%d", queuedBefore, queuedAfter)
	}
	// landed must remain strictly less than queuedAfter — that is the
	// signal persistOnShutdown reads to emit FAILED_DURING_SHUTDOWN. If
	// landed somehow caught up, the WriteFileAtomic-fails-EISDIR
	// invariant has changed and the detection logic is no longer
	// exercised by this test.
	landed := s.lastSavedSeq.Load()
	if landed >= queuedAfter {
		t.Fatalf("expected lastSavedSeq < queuedSeq after write failure; landed=%d queued=%d", landed, queuedAfter)
	}

	// Confirm the directory still exists at storePath (i.e. write really
	// failed; nothing replaced it).
	fi, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat storePath: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("storePath is no longer a directory; WriteFileAtomic unexpectedly succeeded: mode=%v", fi.Mode())
	}
}
