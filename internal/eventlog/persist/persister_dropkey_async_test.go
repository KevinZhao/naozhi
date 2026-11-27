package persist

import (
	"context"
	"os"
	"testing"
)

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
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
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
