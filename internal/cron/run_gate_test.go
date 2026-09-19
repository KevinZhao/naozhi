package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// TestGateNeverUnderTableLock is the machine half of "the gate and the table
// are never held together" (RFC §3.3/§8.5). The reviews found that invariant
// true today by inspection and guaranteed by nothing; with runGate now a type
// of its own, this pins it the same way TestEntryCommitNeverUnderRegistryLock
// pins the commit discipline — a hook at every gate entry, TryLock as the
// witness, and serial execution so the only goroutine that could hold the
// registry lock (or entryMu) at hook time is the one entering the gate.
//
// Not t.Parallel; the hook is package-level.
func TestGateNeverUnderTableLock(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c1", ChatType: "direct"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// The assertion is jobTable.mu only, and the first run of this test is why:
	// it also asserted entryMu and FAILED, because DeleteJobByID legitimately
	// reclaims the gate entry while holding entryMu — its transaction has to, or
	// every deleted job leaks a *runInflight until the next delete sweeps it.
	// That nesting is one-directional (entryMu → gate) and the reverse cannot
	// occur: the gate is a leaf, its methods call nothing that could want
	// entryMu. So entryMu → gate is the documented lock order, not a violation.
	gateOps := 0
	gateHook = func() {
		gateOps++
		if !s.mu.TryLock() {
			t.Error("gate entered with the registry lock held: jobTable.mu and runGate nested")
			return
		}
		s.mu.Unlock()
	}
	t.Cleanup(func() { gateHook = nil })

	// The three production paths that enter the gate, serially.
	inflight, won := s.acquire(j.ID) // execAcquireSlot / dispatchReplay shape
	if !won {
		t.Fatal("acquire lost on an idle job")
	}
	if _, again := s.acquire(j.ID); again {
		t.Fatal("second acquire won while the first still holds the slot")
	}
	inflight.running.Store(false)

	if _, err := s.DeleteJobByID(j.ID); err != nil { // deleteJobPostCleanup → cleanupRunningJobIfIdle
		t.Fatalf("DeleteJobByID: %v", err)
	}

	if gateOps < 3 {
		t.Errorf("hook saw %d gate entries, want ≥3 (two acquires + delete's reclaim) — a path bypassed the gate", gateOps)
	}
}

// TestRunGate_AcquireReleaseReclaim pins the gate's own lifecycle without the
// scheduler around it: win, lose while held, reclaim refuses while held,
// reclaim succeeds once released, and the entry is actually gone.
func TestRunGate_AcquireReleaseReclaim(t *testing.T) {
	t.Parallel()
	var g runGate
	const id = "0123456789abcdef"

	inf, won := g.acquire(id)
	if !won {
		t.Fatal("first acquire lost")
	}
	if _, again := g.acquire(id); again {
		t.Fatal("overlapping acquire won")
	}
	if g.cleanupRunningJobIfIdle(id) {
		t.Fatal("reclaim removed a held gate — a racing acquire would LoadOrStore a fresh one and double-run (#1706)")
	}
	inf.running.Store(false)
	if !g.cleanupRunningJobIfIdle(id) {
		t.Fatal("reclaim refused an idle gate; deleted jobs would leak *runInflight forever (#758)")
	}
	if _, ok := g.peek(id); ok {
		t.Fatal("peek found an entry after reclaim")
	}
	// And the slot is winnable again after reclaim.
	if _, rewon := g.acquire(id); !rewon {
		t.Fatal("acquire lost after reclaim")
	}
}

// TestRunGate_AcquireSerialisedAgainstGate is the missing half of
// TestJobGate_CleanupSerialisedAgainstGate, and the mutation run for this PR is
// why it exists: removing acquire's shard-lock hold turned NOTHING red — the
// whole package passed, including the #1706 race test, whose own comment admits
// it is probabilistic ("the bug window is one instruction wide") and whose real
// value is the -race check, which an unlocked acquire does not trip (atomics
// and sync.Map are individually race-clean; the bug is the interleaving).
//
// Same deterministic technique as the cleanup-side test, pointed the other way:
// hold the shard mutex, and acquire for the same jobID must block on it. If it
// completes, it is not taking the gate, and #1706's window — cleanup deletes
// the entry between acquire's load and CAS, a second acquire LoadOrStores a
// fresh gate, both CAS-win, double execution — is open again.
func TestRunGate_AcquireSerialisedAgainstGate(t *testing.T) {
	t.Parallel()
	var g runGate
	const id = "job-acquire-serialise"

	gate := g.jobGateLock(id)
	gate.Lock()

	type res struct {
		inf *runInflight
		won bool
	}
	done := make(chan res, 1)
	go func() {
		inf, won := g.acquire(id)
		done <- res{inf, won}
	}()

	select {
	case <-done:
		gate.Unlock()
		t.Fatal("acquire completed while the shard gate was held; it is not taking the gate (#1706's window is open)")
	case <-time.After(100 * time.Millisecond):
		// Expected: blocked on the shard mutex.
	}

	gate.Unlock()
	select {
	case r := <-done:
		if !r.won {
			t.Fatal("acquire lost on an idle job after the gate was released")
		}
		r.inf.running.Store(false)
	case <-time.After(2 * time.Second):
		t.Fatal("acquire never completed after gate release")
	}
}
