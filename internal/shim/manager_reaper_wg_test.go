package shim

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestStopAll_WaitsForReaperGoroutines verifies R216-GO-6 (#565):
// StopAll's bounded wait covers the per-shim cmd.Wait() reaper goroutines
// tracked via Manager.reaperWG, not just the per-handle Shutdown sends.
//
// The test exercises the contract directly without spawning a real shim:
// it adds a fake reaper to reaperWG, calls StopAll(ctx) with a tight
// deadline, and asserts StopAll returns within the deadline (the WaitGroup
// path must not block past ctx). It then releases the fake reaper and
// asserts StopAll could not have returned via the wg-drain path before
// release.
func TestStopAll_WaitsForReaperGoroutines(t *testing.T) {
	m := &Manager{shims: make(map[string]*ShimHandle)}

	// Park a fake "reaper" inside reaperWG. StopAll's done-channel goroutine
	// blocks on m.reaperWG.Wait() so it must NOT close `done` until we
	// release this Add. ctx-deadline is the only escape.
	m.reaperWG.Add(1)
	release := make(chan struct{})
	var releasedOnce sync.Once
	releaseFn := func() {
		releasedOnce.Do(func() {
			close(release)
			m.reaperWG.Done()
		})
	}
	defer releaseFn()

	go func() {
		<-release
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	m.StopAll(ctx)
	elapsed := time.Since(start)

	// StopAll must return when ctx expires even though reaperWG is still
	// held — otherwise systemd shutdown could hang forever on a stuck reaper.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("StopAll did not return within ctx-deadline budget: %v", elapsed)
	}
	if elapsed < 40*time.Millisecond {
		// Sanity check: it must have at least waited for ctx.
		t.Fatalf("StopAll returned too early (%v) — reaperWG.Wait was bypassed", elapsed)
	}
}

// TestStopAll_DrainsWhenReapersExit verifies the happy path: when the
// reaper goroutines complete before ctx expiry, StopAll returns via the
// wg-drain branch rather than the ctx-expired branch.
func TestStopAll_DrainsWhenReapersExit(t *testing.T) {
	m := &Manager{shims: make(map[string]*ShimHandle)}

	// Schedule a fake reaper that exits well within the ctx window.
	m.reaperWG.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		m.reaperWG.Done()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	m.StopAll(ctx)
	elapsed := time.Since(start)

	// Drain branch: bounded by reaper exit time, not ctx.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("StopAll did not drain promptly: %v (reaperWG should have completed in ~10ms)", elapsed)
	}
}
