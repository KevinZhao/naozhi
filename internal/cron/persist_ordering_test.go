package cron

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestSaveMarshaledSeq_DropsStaleSeq is the R48-REL-PERSIST-ORDERING-RACE
// regression: if a later writer (larger seq) has already landed its payload,
// an older writer (smaller seq) arriving late must NOT overwrite it.
//
// Without the seq gate, Go's non-FIFO sync.Mutex allowed the following
// schedule to roll back persisted state:
//
//	T1: persistJobsLocked → data_A (seq=1) → release s.mu → wait storeMu
//	T2: persistJobsLocked → data_B (seq=2) → release s.mu → wait storeMu
//	T2: acquires storeMu first → writes data_B → releases
//	T1: acquires storeMu → writes data_A (STALE)   ← old bug
//	disk now reflects seq=1 state, not seq=2
//
// The seq gate makes the T1 write a no-op because lastSavedSeq=2 >= 1.
func TestSaveMarshaledSeq_DropsStaleSeq(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	s := &Scheduler{storePath: path}

	// Simulate T2 winning first: write payload B (seq=2) via the seq path.
	payloadB := []byte(`{"generation":"B"}`)
	s.saveMarshaledSeq(payloadB, 2)
	// Sanity: disk has B.
	if got, err := os.ReadFile(path); err != nil || string(got) != string(payloadB) {
		t.Fatalf("after seq=2 write: got %q err=%v, want %q", got, err, payloadB)
	}

	// Now T1 arrives late with payload A (seq=1). Must be dropped.
	payloadA := []byte(`{"generation":"A-STALE"}`)
	s.saveMarshaledSeq(payloadA, 1)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after stale write: %v", err)
	}
	if string(got) != string(payloadB) {
		t.Errorf("stale seq=1 write clobbered newer seq=2 payload: got %q want %q",
			got, payloadB)
	}
}

// TestSaveMarshaledSeq_AcceptsAdvancingSeq confirms the gate is not over-
// strict: each successive seq lands, matching the happy case where writers
// arrive at storeMu in monotonic order.
func TestSaveMarshaledSeq_AcceptsAdvancingSeq(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	s := &Scheduler{storePath: path}

	s.saveMarshaledSeq([]byte(`{"v":1}`), 1)
	s.saveMarshaledSeq([]byte(`{"v":2}`), 2)
	s.saveMarshaledSeq([]byte(`{"v":3}`), 3)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"v":3}` {
		t.Errorf("want latest seq=3 payload on disk, got %q", got)
	}
	if last := s.lastSavedSeq.Load(); last != 3 {
		t.Errorf("lastSavedSeq = %d, want 3", last)
	}
}

// TestSaveMarshaledSeq_EqualSeqIsDropped verifies the `seq <= last` boundary:
// two writers sharing the same seq (can't happen in production because
// persistJobsLocked.Add(1) is monotonic, but contract-test the gate) do not
// both land. This matters if a future refactor ever reuses a seq value.
func TestSaveMarshaledSeq_EqualSeqIsDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	s := &Scheduler{storePath: path}

	s.saveMarshaledSeq([]byte(`{"first":true}`), 5)
	// Equal seq — must be dropped (newer-OR-equal landed already).
	s.saveMarshaledSeq([]byte(`{"second":true}`), 5)

	got, _ := os.ReadFile(path)
	if string(got) != `{"first":true}` {
		t.Errorf("equal-seq write overwrote; got %q", got)
	}
}

// TestSaveMarshaledSeq_ConcurrentWritersNoRollback stresses the gate with
// 50 concurrent writers assigned seq=1..50 in arbitrary goroutine schedule.
// After Wait, the disk MUST hold exactly seq=50's payload. Run with -race
// to catch any data race on lastSavedSeq / storeMu.
func TestSaveMarshaledSeq_ConcurrentWritersNoRollback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_jobs.json")
	s := &Scheduler{storePath: path}

	const N = 50
	var wg sync.WaitGroup
	for i := 1; i <= N; i++ {
		wg.Add(1)
		seq := uint64(i)
		payload := []byte{'{', '"', 's', '"', ':', byte('0' + (i % 10)), '}'}
		// Dispatch out of order by interleaving odd/even.
		go func() {
			defer wg.Done()
			s.saveMarshaledSeq(payload, seq)
		}()
	}
	wg.Wait()

	if last := s.lastSavedSeq.Load(); last != N {
		t.Errorf("lastSavedSeq = %d, want %d (some writer's CAS got lost)", last, N)
	}
	// The disk payload must be the seq=N payload; any stale arrival after
	// seq=N landed should have been dropped.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	wantByte := byte('0' + (N % 10))
	if len(got) < 6 || got[5] != wantByte {
		t.Errorf("disk payload = %q does not end with seq=%d byte %q",
			got, N, string(wantByte))
	}
}
