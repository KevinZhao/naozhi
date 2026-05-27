package persist

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPersister_DropKey_AsyncRemoveStillWaits pins R20260527-PERF-4 (#1284):
// the DropKey API must still appear synchronous to the caller — the file
// removal goroutine writes to the done channel after os.Remove completes,
// so the caller's <-done arm in DropKey blocks until cleanup is finished.
// Even though the writer goroutine is freed earlier, the caller observes
// the same end-state (files gone, no error) it did pre-fix.
func TestPersister_DropKey_AsyncRemoveStillWaits(t *testing.T) {
	p, dir := newTestPersister(t)
	sink := p.SinkFor("k1")
	sink([]Entry{entry(t, 1, "u1")}, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if _, err := os.Stat(LogPath(dir, "k1")); err != nil {
		t.Fatalf("log missing pre-drop: %v", err)
	}
	if err := p.DropKey(ctx, "k1"); err != nil {
		t.Fatalf("DropKey: %v", err)
	}
	// Synchronously after DropKey returns, both files MUST be gone — the
	// async unlink goroutine writes to done only after os.Remove has run.
	if _, err := os.Stat(LogPath(dir, "k1")); !os.IsNotExist(err) {
		t.Errorf("log still exists post-DropKey: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyHash("k1")+idxExt)); !os.IsNotExist(err) {
		t.Errorf("idx still exists post-DropKey: err=%v", err)
	}
}

// TestPersister_DropKey_RemovesInMemoryWriterSynchronously asserts the
// in-memory phase (close fd + delete map entry) completes by the time
// the writer goroutine moves on. After DropKey returns, a fresh SinkFor
// for the same key must observe a clean slate (no pre-existing writer
// in p.writers) and re-create the on-disk files.
func TestPersister_DropKey_RemovesInMemoryWriterSynchronously(t *testing.T) {
	p, dir := newTestPersister(t)

	// Round 1: write + drop.
	sink1 := p.SinkFor("k2")
	sink1([]Entry{entry(t, 1, "u1")}, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush 1: %v", err)
	}
	if err := p.DropKey(ctx, "k2"); err != nil {
		t.Fatalf("DropKey: %v", err)
	}

	// Round 2: re-sink for the same key. The persister must build a
	// fresh writer (the post-drop state had the map entry removed
	// synchronously, so the next batch hits the open-writer path).
	sink2 := p.SinkFor("k2")
	sink2([]Entry{entry(t, 2, "u2")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush 2: %v", err)
	}
	// Files must exist again after Round 2.
	if _, err := os.Stat(LogPath(dir, "k2")); err != nil {
		t.Errorf("log missing after re-sink: %v", err)
	}
}
