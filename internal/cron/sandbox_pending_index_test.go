package cron

// Tests for R20260616-PERF-001 (#2140): stopSandboxRunsForJob resolves a
// job's in-flight pending file via the write-authoritative
// sandboxPendingIndex (single map lookup) instead of scanning + unmarshalling
// every concurrent run's record. The index falls back to the dir scan only on
// an index miss (e.g. a record left by a previous process).

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSandboxPendingIndex_ConcurrentReadWrite is a -race canary for
// [R202606-PERF-001]: lookupSandboxPendingIndex takes the read lock while
// set/clear take the write lock on the same sync.RWMutex. Many concurrent
// readers racing a writer must surface any lock-discipline regression (e.g. a
// reader downgraded to a plain map read without holding RLock) under -race.
func TestSandboxPendingIndex_ConcurrentReadWrite(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	const jobID = "0123456789abcdef"
	const path = "/tmp/sandboxpending/feedfacefeedface.json"

	var wg sync.WaitGroup
	// Writers: flip the entry in and out.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.setSandboxPendingIndex(jobID, path)
			s.clearSandboxPendingIndex(jobID, path)
		}
	}()
	// Readers: many concurrent RLock lookups.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if got := s.lookupSandboxPendingIndex(jobID); got != "" && got != path {
					t.Errorf("unexpected index value %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestSandboxPendingIndex_WriteThenLookup pins that writeSandboxPending
// populates the index and the terminal clear removes it.
func TestSandboxPendingIndex_WriteThenLookup(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, storePath)

	jobID := "0123456789abcdef"
	p := sandboxPending{
		JobID:            jobID,
		RunID:            "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	}

	if got := s.lookupSandboxPendingIndex(jobID); got != "" {
		t.Fatalf("index lookup before write = %q, want \"\"", got)
	}

	path := s.writeSandboxPending(p, slog.Default())
	if path == "" {
		t.Fatal("writeSandboxPending returned empty path")
	}
	if got := s.lookupSandboxPendingIndex(jobID); got != path {
		t.Fatalf("index lookup after write = %q, want %q", got, path)
	}

	// Path-guarded clear: a stale (non-matching) path must not evict the entry.
	s.clearSandboxPendingIndex(jobID, "/some/other/path.json")
	if got := s.lookupSandboxPendingIndex(jobID); got != path {
		t.Fatalf("non-matching clear evicted the entry: got %q, want %q", got, path)
	}
	// Matching clear evicts.
	s.clearSandboxPendingIndex(jobID, path)
	if got := s.lookupSandboxPendingIndex(jobID); got != "" {
		t.Fatalf("index lookup after matching clear = %q, want \"\"", got)
	}
}

// TestStopSandboxRunsForJob_IndexFastPath pins that a job whose pending record
// was written via writeSandboxPending (and thus indexed) is Stopped and its
// file removed + index entry cleared on delete.
func TestStopSandboxRunsForJob_IndexFastPath(t *testing.T) {
	tests := []struct {
		name        string
		stopErr     error
		wantStops   int
		wantRemoved bool
		wantIndexed bool // index entry still present after the delete
	}{
		{
			name:        "confirmed stop removes file and clears index",
			stopErr:     nil,
			wantStops:   1,
			wantRemoved: true,
			wantIndexed: false,
		},
		{
			name:        "failed stop keeps file and index for retry",
			stopErr:     errors.New("api down"),
			wantStops:   1,
			wantRemoved: false,
			wantIndexed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			storePath := filepath.Join(dir, "cron_jobs.json")
			runner := &fakeSandboxRunner{stopErr: tc.stopErr}
			s, _ := sandboxTestScheduler(t, runner, storePath)

			jobID := "0123456789abcdef"
			path := s.writeSandboxPending(sandboxPending{
				JobID:            jobID,
				RunID:            "feedfacefeedface",
				RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
				StartedAtMS:      time.Now().UnixMilli(),
			}, slog.Default())
			if path == "" {
				t.Fatal("writeSandboxPending returned empty path")
			}

			s.stopSandboxRunsForJob(jobID)

			runner.mu.Lock()
			nStops := len(runner.stopped)
			runner.mu.Unlock()
			if nStops != tc.wantStops {
				t.Fatalf("StopSession calls = %d, want %d", nStops, tc.wantStops)
			}

			_, statErr := os.Stat(path)
			removed := errors.Is(statErr, os.ErrNotExist)
			if removed != tc.wantRemoved {
				t.Fatalf("file removed = %v, want %v", removed, tc.wantRemoved)
			}

			indexed := s.lookupSandboxPendingIndex(jobID) != ""
			if indexed != tc.wantIndexed {
				t.Fatalf("index entry present = %v, want %v", indexed, tc.wantIndexed)
			}
		})
	}
}

// TestStopSandboxRunsForJob_SlowPathScanOnIndexMiss pins that a pending record
// that is NOT in the in-memory index (e.g. written by a previous process, as
// the fixture helper simulates) is still found via the dir-scan fallback.
func TestStopSandboxRunsForJob_SlowPathScanOnIndexMiss(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	jobID := "0123456789abcdef"
	// writePendingFixture writes the file WITHOUT populating the index —
	// exactly the previous-process-orphan scenario.
	path := writePendingFixture(t, storePath, sandboxPending{
		JobID:            jobID,
		RunID:            "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})
	if s.lookupSandboxPendingIndex(jobID) != "" {
		t.Fatal("precondition: index must be empty for the slow-path test")
	}

	s.stopSandboxRunsForJob(jobID)

	runner.mu.Lock()
	nStops := len(runner.stopped)
	runner.mu.Unlock()
	if nStops != 1 {
		t.Fatalf("slow-path StopSession calls = %d, want 1", nStops)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("slow-path: pending file must be removed after confirmed Stop")
	}
}
