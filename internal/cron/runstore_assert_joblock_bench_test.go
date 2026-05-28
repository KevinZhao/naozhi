package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkRunStore_AssertJobLockHeld measures the per-call overhead of
// the assertJobLockHeld contract probe so reviewers can compare the
// testing.Testing()-gated path against the pre-fix unconditional
// TryLock+Unlock variant. Under `go test -bench`, testing.Testing()
// returns true and the probe still fires (so this benchmark reflects
// the test-mode floor, NOT the production zero-cost path); the
// comparator value is the *delta* you'd see after toggling the gate
// on a non-test build target. R249-CR-18 (#961).
//
// Run: `go test -bench=BenchmarkRunStore_AssertJobLockHeld
// -run=^$ ./internal/cron/...`
func BenchmarkRunStore_AssertJobLockHeld(b *testing.B) {
	tmp := b.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := newRunStore(storePath, 10, time.Hour)
	if s == nil || s.disabled {
		b.Fatalf("newRunStore must succeed; got disabled")
	}
	jobID := "0123456789abcdef"
	// Warm the jobLock map so the first iteration does not pay the
	// sync.Map.LoadOrStore allocation; we want to measure the probe
	// itself, not lazy-init.
	_ = s.jobLock(jobID)
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.assertJobLockHeld(jobID)
	}
}
