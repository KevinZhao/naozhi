// running_jobs_cleanup_compare_delete_test.go pins R20260527-GO-2 (#1270):
// cleanupRunningJobIfIdle must not race a concurrent jobInflight() that
// has rotated the *runInflight pointer (e.g. via the AddJob ID-reuse
// path). LoadAndDelete would erase the freshly-stored pointer; the
// new CompareAndDelete shape leaves it intact.
package cron

import (
	"os"
	"strings"
	"testing"
)

// TestCleanupRunningJobIfIdle_PreservesRotatedPointer simulates the
// split-CAS-gate window: cleanupRunningJobIfIdle has Loaded the OLD
// inflight pointer, observed running == false, but BEFORE it deletes
// a concurrent jobInflight() has stored a FRESH pointer for the same
// jobID. The fix uses CompareAndDelete so the fresh pointer survives.
func TestCleanupRunningJobIfIdle_PreservesRotatedPointer(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, AllowNilRouter: true})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	const jobID = "0123456789abcdef"

	// Stage 1: install an OLD *runInflight in the map and capture its
	// pointer. Since cleanupRunningJobIfIdle reads via Load() and we
	// can't easily inject a "interleave between Load and Delete" hook,
	// we instead manually drive the operations the way the real race
	// would: simulate "Load happened" by explicitly capturing the old
	// pointer, then rotate the map entry to a NEW pointer, then call
	// cleanup which will Load the new pointer but try to delete it
	// based on the comparison the real Load would have observed.
	//
	// The test below is the strictest form: rotate to a NEW pointer
	// AND check that cleanup doesn't blast the new pointer away.
	old := &runInflight{}
	s.runningJobs.Store(jobID, old)

	// Rotate: a concurrent AddJob retry path would LoadOrStore a new
	// guard. Here we simulate by Delete + Store (LoadOrStore would
	// no-op because old is already there).
	s.runningJobs.Delete(jobID)
	fresh := &runInflight{}
	s.runningJobs.Store(jobID, fresh)
	// Mark fresh as busy so cleanup's running.Load() check returns
	// true and we hit the busy-skip path. This is the second line of
	// defence: even before CompareAndDelete, busy-gate skip catches
	// the rotation in most cases.
	fresh.running.Store(true)

	// cleanup observes the fresh pointer (because Load returns the
	// CURRENT map value, not the stale one our hypothetical concurrent
	// reader would have grabbed). The busy-skip prevents delete.
	if s.cleanupRunningJobIfIdle(jobID) {
		t.Errorf("cleanupRunningJobIfIdle: should NOT delete a busy fresh entry")
	}
	if v, ok := s.runningJobs.Load(jobID); !ok || v.(*runInflight) != fresh {
		t.Errorf("fresh entry must survive: got %v ok=%v want %v", v, ok, fresh)
	}

	// Now release the gate; the next cleanup MUST CompareAndDelete
	// based on the current pointer (fresh) and succeed.
	fresh.running.Store(false)
	if !s.cleanupRunningJobIfIdle(jobID) {
		t.Errorf("cleanupRunningJobIfIdle: should delete idle fresh entry")
	}
	if _, ok := s.runningJobs.Load(jobID); ok {
		t.Errorf("entry should be gone after idle cleanup")
	}
}

// TestCleanupRunningJobIfIdle_CompareAndDeleteContract is a structural
// pin: the cleanup helper must use sync.Map.CompareAndDelete so a
// rotated pointer is not erased. Using LoadAndDelete instead would
// surface as the split-CAS race only under heavy load — a unit test
// can't reliably trigger that timing, but a source-level pin catches
// any "simplification" PR that swaps the contract back.
func TestCleanupRunningJobIfIdle_CompareAndDeleteContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("scheduler_run.go")
	if err != nil {
		t.Fatalf("read scheduler_run.go: %v", err)
	}
	body := string(src)
	const fn = "func (s *Scheduler) cleanupRunningJobIfIdle("
	idx := strings.Index(body, fn)
	if idx < 0 {
		t.Fatalf("cleanupRunningJobIfIdle function not found")
	}
	rest := body[idx:]
	if next := strings.Index(rest[len(fn):], "\nfunc "); next >= 0 {
		rest = rest[:len(fn)+next]
	}
	if !strings.Contains(rest, "CompareAndDelete") {
		t.Errorf("cleanupRunningJobIfIdle must use sync.Map.CompareAndDelete on the *runInflight pointer (R20260527-GO-2 / #1270). Reverting to LoadAndDelete reopens the split-CAS-gate window — a concurrent jobInflight() rotating to a fresh pointer would have the rotation silently erased, splitting the CAS gate across two distinct *runInflight values.")
	}
}
