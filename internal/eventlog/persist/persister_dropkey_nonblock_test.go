package persist

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// blockingRemoveHook installs a removeFileHook that blocks the FIRST unlink
// on a release channel and records when it started, so a test can drive the
// exact "stem A is mid-unlink while other work arrives" window
// deterministically. Returns the channel that, when closed, releases the
// blocked unlink, plus a channel closed once the unlink has begun.
func blockingRemoveHook(t *testing.T) (release chan struct{}, started chan struct{}) {
	t.Helper()
	prev := removeFileHook
	release = make(chan struct{})
	started = make(chan struct{})
	var once sync.Once
	var blockOnce sync.Once
	removeFileHook = func(path string) error {
		blockOnce.Do(func() {
			once.Do(func() { close(started) })
			<-release
		})
		return os.Remove(path)
	}
	t.Cleanup(func() { removeFileHook = prev })
	return release, started
}

func logContains(t *testing.T, dir, key, needle string) bool {
	t.Helper()
	recs := readAllRecords(t, LogPath(dir, key))
	for _, r := range recs {
		if len(r.Entry) > 0 && bytes.Contains(r.Entry, []byte(needle)) {
			return true
		}
	}
	return false
}

// TestPersister_DropDoesNotBlockOtherStem pins #1848: while stem A's unlink
// is in flight (slow FUSE/NFS), a batch for a DIFFERENT stem B must be
// persisted promptly and NOT dropped. Before the per-stem-pending fix
// writerFor blocked the single writer goroutine on A's channel, so B's
// batch sat in p.in (and on a busy host got dropped once the channel
// saturated). Here we assert B reaches disk while A is still blocked.
func TestPersister_DropDoesNotBlockOtherStem(t *testing.T) {
	release, started := blockingRemoveHook(t)
	p, dir := newTestPersister(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed key A so it has files on disk to remove.
	p.SinkFor("keyA")([]Entry{entry(t, 1, "a1")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush A: %v", err)
	}

	// Kick off DropKey(A) in the background; its async unlink will block on
	// `release` so the dropping entry for stem A stays installed.
	dropErr := make(chan error, 1)
	go func() { dropErr <- p.DropKey(ctx, "keyA") }()

	// Wait until the unlink has actually started (dropState installed,
	// goroutine parked in the hook).
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("unlink never started")
	}

	// Now send + flush key B. If run were blocked on A's unlink this Flush
	// would not complete until release; we keep A blocked the whole time.
	beforeReleased := time.Now()
	p.SinkFor("keyB")([]Entry{entry(t, 2, "b1")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush B (writer blocked on A's unlink?): %v", err)
	}
	if time.Since(beforeReleased) > 2*time.Second {
		t.Fatalf("Flush B took too long; run goroutine likely blocked on A unlink")
	}

	// B must be durable on disk while A is still unlinking.
	if !logContains(t, dir, "keyB", "b1") {
		t.Fatalf("key B entry not persisted while A's unlink was in flight")
	}

	// Release A's unlink and let DropKey complete.
	close(release)
	select {
	case err := <-dropErr:
		if err != nil {
			t.Fatalf("DropKey A: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DropKey A never returned after release")
	}
}

// TestPersister_DropThenRecreate_DeferredReplay pins the #1848 deferral path:
// a batch that arrives for stem A WHILE A is mid-unlink is deferred into the
// per-stem pending FIFO and replayed (not dropped) once the unlink finishes,
// recreating the file with the deferred entry.
func TestPersister_DropThenRecreate_DeferredReplay(t *testing.T) {
	release, started := blockingRemoveHook(t)
	p, dir := newTestPersister(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.SinkFor("rk")([]Entry{entry(t, 1, "orig")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush orig: %v", err)
	}

	dropErr := make(chan error, 1)
	go func() { dropErr <- p.DropKey(ctx, "rk") }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("unlink never started")
	}

	// Send a fresh batch for the SAME key while the unlink is blocked. It
	// must be deferred, not dropped, and not lost.
	p.SinkFor("rk")([]Entry{entry(t, 2, "deferred")}, false)

	// Release the unlink; opDropDone replays the deferred batch.
	close(release)
	if err := <-dropErr; err != nil {
		t.Fatalf("DropKey: %v", err)
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush after replay: %v", err)
	}

	if _, err := os.Stat(LogPath(dir, "rk")); err != nil {
		t.Fatalf("recreated log missing after deferred replay: %v", err)
	}
	if !logContains(t, dir, "rk", "deferred") {
		t.Fatalf("deferred entry was not replayed onto the recreated file")
	}
}

// TestPersister_DeferredReplay_Ordering pins that multiple batches deferred
// behind one unlink replay in arrival order (FIFO), preserving the per-key
// event sequence the recovery layer relies on.
func TestPersister_DeferredReplay_Ordering(t *testing.T) {
	release, started := blockingRemoveHook(t)
	p, dir := newTestPersister(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.SinkFor("ord")([]Entry{entry(t, 1, "seed")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush seed: %v", err)
	}

	dropErr := make(chan error, 1)
	go func() { dropErr <- p.DropKey(ctx, "ord") }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("unlink never started")
	}

	// Three batches deferred while the unlink is blocked. Each SinkFor.accept
	// is a synchronous channel send and the run goroutine drains p.in FIFO,
	// so handleBatch sees them in order; assert the on-disk order matches.
	uuids := []string{"d-one", "d-two", "d-three"}
	for i, u := range uuids {
		p.SinkFor("ord")([]Entry{entry(t, int64(10+i), u)}, false)
		// Give the run goroutine a beat to drain p.in into pending in order.
		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	if err := <-dropErr; err != nil {
		t.Fatalf("DropKey: %v", err)
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush after replay: %v", err)
	}

	recs := readAllRecords(t, LogPath(dir, "ord"))
	var seen []string
	for _, r := range recs {
		for _, u := range uuids {
			if len(r.Entry) > 0 && bytes.Contains(r.Entry, []byte(u)) {
				seen = append(seen, u)
			}
		}
	}
	if len(seen) != len(uuids) {
		t.Fatalf("expected all %d deferred entries replayed, got %v", len(uuids), seen)
	}
	for i := range uuids {
		if seen[i] != uuids[i] {
			t.Fatalf("deferred replay out of order: got %v want %v", seen, uuids)
		}
	}
}

// TestPersister_StopDuringDrop replays deferred batches on a clean Stop so a
// shutdown that races an in-flight unlink does not silently lose the deferred
// events. The unlink is released only after Stop begins draining.
func TestPersister_StopDuringDrop(t *testing.T) {
	release, started := blockingRemoveHook(t)
	p, dir := newTestPersister(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p.SinkFor("sk")([]Entry{entry(t, 1, "seed")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush seed: %v", err)
	}

	dropErr := make(chan error, 1)
	go func() { dropErr <- p.DropKey(ctx, "sk") }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("unlink never started")
	}

	// Defer a batch behind the in-flight unlink, then release so opDropDone
	// posts; immediately Stop. Whether the replay happens via the live
	// opDropDone or via Stop's replayDroppingPending drain, the deferred
	// entry must survive.
	p.SinkFor("sk")([]Entry{entry(t, 2, "stopdeferred")}, false)
	time.Sleep(5 * time.Millisecond)
	close(release)
	if err := <-dropErr; err != nil {
		t.Fatalf("DropKey: %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !logContains(t, dir, "sk", "stopdeferred") {
		t.Fatalf("deferred entry lost across Stop-during-drop")
	}
}
