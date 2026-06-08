package cron

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// R20260607-PERF-005 (#1923): recordTerminalResult no longer runs json.Marshal
// inside the s.mu write critical section. These tests pin (1) the snapshot
// helper produces the same byte output as the in-lock marshal path, (2) the
// snapshot is a detached value copy immune to post-unlock mutation, and (3) the
// finish path's marshal runs while s.mu is free (not serialised behind the
// write lock).

// marshalJobsSnapshotForTest encodes a detached snapshot through the same
// serializer persistSnapshot uses, returning the bytes directly so tests can
// assert on-disk shape without driving the async save closure.
func (s *Scheduler) marshalJobsSnapshotForTest(snap jobsSnapshot) ([]byte, error) {
	if fn := s.marshalJobs.Load(); fn != nil {
		return (*fn)(snap.entries)
	}
	return defaultMarshalJobs(snap.entries)
}

// TestSnapshotJobsForSaveLocked_MatchesMarshalLocked pins on-disk byte parity
// between the new off-lock snapshot+marshal path and the legacy in-lock
// marshalJobsLocked path. cron_jobs.json shape must not drift regardless of
// which persist path produced it.
func TestSnapshotJobsForSaveLocked_MatchesMarshalLocked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	notify := true
	for _, j := range []*Job{
		{Schedule: "@every 1h", Prompt: "zeta", Platform: "feishu", ChatID: "c1", ChatType: "direct", Paused: true},
		{Schedule: "@every 2h", Prompt: "alpha", Platform: "feishu", ChatID: "c2", ChatType: "direct", Paused: true, Notify: &notify},
		{Schedule: "@every 3h", Prompt: "mike", Platform: "feishu", ChatID: "c3", ChatType: "direct", Paused: true},
	} {
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}

	s.mu.RLock()
	wantBytes, err := s.marshalJobsLocked()
	if err != nil {
		s.mu.RUnlock()
		t.Fatalf("marshalJobsLocked: %v", err)
	}
	snap := s.snapshotJobsForSaveLocked()
	s.mu.RUnlock()

	gotBytes, err := s.marshalJobsSnapshotForTest(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("snapshot marshal diverged from in-lock marshal:\n got=%s\nwant=%s", gotBytes, wantBytes)
	}
}

// TestSnapshotJobsForSaveLocked_Detached verifies the snapshot is a value copy
// that does NOT observe a mutation applied after s.mu is released. This is the
// correctness guarantee that lets json.Marshal run off the lock without racing
// a concurrent mutator.
func TestSnapshotJobsForSaveLocked_Detached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{ID: "abcd1234", Schedule: "@every 1h", Prompt: "original", Platform: "feishu", ChatID: "c1", ChatType: "direct", Paused: true}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.sortedJobIDs = append(s.sortedJobIDs, j.ID)
	snap := s.snapshotJobsForSaveLocked()
	s.mu.Unlock()

	// Mutate the live job AFTER the snapshot was taken and the lock dropped.
	s.mu.Lock()
	j.Prompt = "MUTATED"
	s.mu.Unlock()

	data, err := s.marshalJobsSnapshotForTest(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var rt []*Job
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, data)
	}
	if len(rt) != 1 {
		t.Fatalf("want 1 job, got %d", len(rt))
	}
	if rt[0].Prompt != "original" {
		t.Fatalf("snapshot leaked post-unlock mutation: Prompt=%q want %q", rt[0].Prompt, "original")
	}
}

// TestRecordTerminalResult_MarshalRunsOffLock proves json.Marshal no longer
// runs inside the s.mu write critical section: while a deliberately slow
// marshaler is encoding, another goroutine must be able to acquire s.mu. Before
// #1923 the marshal ran under the write lock and this probe would block for the
// full encode duration.
func TestRecordTerminalResult_MarshalRunsOffLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		s.marshalJobs.Store(&defaultMarshalJobs)
		s.Stop()
	})

	j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c1", ChatType: "direct", Paused: true}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	marshalEntered := make(chan struct{})
	releaseMarshal := make(chan struct{})
	var entered atomic.Bool
	slow := marshalJobsFn(func(v any) ([]byte, error) {
		if entered.CompareAndSwap(false, true) {
			close(marshalEntered)
			<-releaseMarshal
		}
		return json.Marshal(v)
	})
	s.marshalJobs.Store(&slow)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.recordTerminalResult(j, "result", "", "sess", ErrClassNone, RunStateSucceeded, time.Now())
	}()

	// Wait until the slow marshaler is mid-encode.
	select {
	case <-marshalEntered:
	case <-time.After(2 * time.Second):
		close(releaseMarshal)
		wg.Wait()
		t.Fatal("slow marshaler never entered — recordTerminalResult did not reach marshal")
	}

	// s.mu must be free while marshal is in-flight (it runs off the lock now).
	lockAcquired := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.mu.Unlock()
		close(lockAcquired)
	}()
	select {
	case <-lockAcquired:
		// Lock was free during the off-lock marshal — #1923 holds.
	case <-time.After(2 * time.Second):
		close(releaseMarshal)
		wg.Wait()
		t.Fatal("s.mu held during marshal — json.Marshal still in the write critical section (#1923 regression)")
	}

	close(releaseMarshal)
	wg.Wait()
}
