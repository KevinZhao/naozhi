package persist

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// TestPersister_DropThenRecreate_SlowRemove pins #1774: an async, slow
// removeKeyFiles for a stem must NOT delete the files of a same-key
// session recreated right after DropKey returns. The fix makes writerFor
// block on the per-stem dropping channel so O_CREATE lands strictly after
// the unlink; here we inject an artificially slow os.Remove to widen the
// window and assert the recreated file survives with its fresh entry.
func TestPersister_DropThenRecreate_SlowRemove(t *testing.T) {
	// Install a slow-remove hook for the duration of this test. Each
	// unlink sleeps long enough that, without the dropping-channel guard,
	// the recreate's OpenFile would win the race and then get clobbered.
	prev := removeFileHook
	var mu sync.Mutex
	slept := false
	removeFileHook = func(path string) error {
		mu.Lock()
		first := !slept
		slept = true
		mu.Unlock()
		if first {
			time.Sleep(150 * time.Millisecond)
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { removeFileHook = prev })

	p, dir := newTestPersister(t)

	// Round 1: write + flush so files exist on disk.
	sink1 := p.SinkFor("racekey")
	sink1([]Entry{entry(t, 1, "u1")}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush 1: %v", err)
	}

	// Round 2: kick off DropKey and, while its slow async unlink is still
	// in flight, recreate the same key from another goroutine. This is the
	// exact #1774 window: without the dropping-channel guard, writerFor's
	// O_CREATE would interleave with the pending os.Remove and the new file
	// would be clobbered. We start the recreate slightly after DropKey so
	// the in-memory drop phase has run but the slow unlink has not.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := p.DropKey(ctx, "racekey"); err != nil {
			t.Errorf("DropKey: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		// Give the drop a moment to enter its slow unlink, then recreate.
		time.Sleep(40 * time.Millisecond)
		sink2 := p.SinkFor("racekey")
		sink2([]Entry{entry(t, 2, "u2")}, false)
	}()
	wg.Wait()

	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush 2: %v", err)
	}

	// The recreated log must exist and contain the fresh entry's uuid —
	// proving the slow unlink did not delete the recreated file.
	logPath := LogPath(dir, "racekey")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("recreated log missing (clobbered by slow remove): %v", err)
	}
	recs := readAllRecords(t, logPath)
	foundU2 := false
	for _, r := range recs {
		if len(r.Entry) > 0 && bytes.Contains(r.Entry, []byte(`"u2"`)) {
			foundU2 = true
		}
	}
	if !foundU2 {
		t.Fatalf("recreated log does not contain fresh entry u2; records=%d", len(recs))
	}
}

// TestPersister_DropThenRecreate_Concurrent stresses the same invariant
// with the recreate racing the drop's done signal from a separate
// goroutine, run under -race to catch any map/channel misuse on the
// dropping bookkeeping. Several drop/recreate cycles interleave.
func TestPersister_DropThenRecreate_Concurrent(t *testing.T) {
	prev := removeFileHook
	removeFileHook = func(path string) error {
		time.Sleep(5 * time.Millisecond)
		return os.Remove(path)
	}
	t.Cleanup(func() { removeFileHook = prev })

	p, dir := newTestPersister(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const key = "ckey"
	for i := 0; i < 8; i++ {
		sink := p.SinkFor(key)
		sink([]Entry{entry(t, int64(i+1), "u")}, false)
		if err := p.Flush(ctx); err != nil {
			t.Fatalf("Flush round %d: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		// Drop and immediate recreate race each other.
		go func() {
			defer wg.Done()
			if err := p.DropKey(ctx, key); err != nil {
				t.Errorf("DropKey round %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			s := p.SinkFor(key)
			s([]Entry{entry(t, 999, "recreate")}, false)
		}()
		wg.Wait()
	}

	// Final recreate + flush must leave a valid, present log file.
	s := p.SinkFor(key)
	s([]Entry{entry(t, 1000, "final")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("final Flush: %v", err)
	}
	if _, err := os.Stat(LogPath(dir, key)); err != nil {
		t.Fatalf("final log missing: %v", err)
	}
}
