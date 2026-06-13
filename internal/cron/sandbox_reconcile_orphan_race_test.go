package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestR20260613GO002_ReconcileOrphanNoRaceWithUpdateJob is a -race regression
// test for R20260613-GO-002: reconcileOneSandboxOrphan previously read
// j.Prompt / j.FreshContext / j.SideEffects / j.Title outside the RLock,
// racing with concurrent UpdateJob calls that mutate those fields under
// s.mu.Lock (and may execute *j = preUpdate for rollback).
//
// Strategy: launch N goroutines calling UpdateJob in a tight loop while the
// main goroutine calls reconcileOneSandboxOrphan on the same job. The Go race
// detector will report a DATA RACE if any field is read without the lock held.
func TestR20260613GO002_ReconcileOrphanNoRaceWithUpdateJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")

	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	j := sandboxJob(t, s)

	// Write a pending record pointing at this job so reconcileOneSandboxOrphan
	// has a real job to look up (exercises the field-snapshot path).
	pendingDir := filepath.Join(dir, "sandboxpending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := sandboxPending{
		JobID:            j.ID,
		RunID:            "aabbccddeeff0011",
		RuntimeSessionID: "run-aabbccddeeff0011-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	}
	raw, _ := json.Marshal(p)
	pendingPath := filepath.Join(pendingDir, p.RunID+".json")
	if err := os.WriteFile(pendingPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	const workers = 4
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent UpdateJob goroutines mutating the fields that
	// reconcileOneSandboxOrphan reads. Note: sandbox jobs reject WorkDir,
	// so we only mutate the fields that are actually allowed.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			prompts := []string{"prompt-a", "prompt-b", "prompt-c"}
			fresh := i%2 == 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				prompt := prompts[i%len(prompts)]
				sideEff := i%3 == 0
				//nolint:errcheck // best-effort in race test
				_, _ = s.UpdateJob(j.ID, JobUpdate{
					Prompt:       &prompt,
					FreshContext: &fresh,
					SideEffects:  &sideEff,
				})
			}
		}()
	}

	// reconcileOneSandboxOrphan reads j fields: before the fix they were read
	// outside RLock; with the fix all reads are snapshotted inside RLock.
	// Re-write the pending file each iteration so reconcile has a real path.
	for iter := 0; iter < 50; iter++ {
		raw, _ := json.Marshal(p)
		_ = os.WriteFile(pendingPath, raw, 0o600)
		s.reconcileOneSandboxOrphan(p, pendingPath)
	}

	close(stop)
	wg.Wait()
}

// TestR20260613GO002_ReconcileOrphanSnapshotSemantics verifies the snapshot
// path reaches a terminal record with the correct error class, confirming
// that the snapshotted fields are used properly in finishRun.
func TestR20260613GO002_ReconcileOrphanSnapshotSemantics(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	// Set known field values before reconcile.
	prompt := "known-prompt"
	fresh := true
	if _, err := s.UpdateJob(j.ID, JobUpdate{
		Prompt:       &prompt,
		FreshContext: &fresh,
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	pendingDir := filepath.Join(dir, "sandboxpending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := sandboxPending{
		JobID:       j.ID,
		RunID:       "1122334455667788",
		StartedAtMS: time.Now().Add(-1 * time.Minute).UnixMilli(),
	}
	raw, _ := json.Marshal(p)
	path := filepath.Join(pendingDir, p.RunID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s.reconcileOneSandboxOrphan(p, path)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rec.endedCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if rec.endedCount() == 0 {
		t.Fatal("expected a terminal cron_run_ended event from reconcile")
	}
	ev := rec.endedAtCron(0)
	if ev.RunID != p.RunID {
		t.Errorf("terminal run id = %q, want %q", ev.RunID, p.RunID)
	}
	if ev.ErrorClass != ErrClassSandboxTransport {
		t.Errorf("error_class = %q, want sandbox_transport", ev.ErrorClass)
	}
}
