package cron

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

// TestRunStore_NullObject_DisabledStore guards R249-ARCH-29 (#993): after the
// caller-side `s.runStore == nil` guards were dropped in favour of the runStore
// null-object (newRunStore always returns a non-nil &runStore{disabled:true}
// when StorePath is empty, and every method is nil-receiver + disabled safe),
// the Scheduler read methods must still return the persistence-off zero values
// without panicking.
func TestRunStore_NullObject_DisabledStore(t *testing.T) {
	rs := newRunStore("", 0, 0) // empty StorePath => disabled null-object
	if rs == nil {
		t.Fatal("newRunStore returned nil; null-object contract broken")
	}
	if !rs.disabled {
		t.Fatal("newRunStore with empty StorePath must be disabled")
	}

	s := &Scheduler{runStore: rs}

	if got := s.ListRuns("job1", 10, time.Time{}); got != nil {
		t.Fatalf("ListRuns on disabled store = %v, want nil", got)
	}
	if got := s.RecentRuns("job1", 5); got != nil {
		t.Fatalf("RecentRuns on disabled store = %v, want nil", got)
	}
	if _, err := s.GetRun("job1", "run1"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("GetRun on disabled store err = %v, want fs.ErrNotExist", err)
	}
}

// TestRunStore_NullObject_NilReceiverMethodsSafe pins that the *runStore
// methods the dropped guards relied on are genuinely nil-receiver safe, so a
// zero-value Scheduler (runStore nil — only reachable in tests / future
// mis-construction) still cannot panic through the un-guarded call sites.
func TestRunStore_NullObject_NilReceiverMethodsSafe(t *testing.T) {
	var rs *runStore // nil

	if got := rs.List("j", 1, time.Time{}); got != nil {
		t.Fatalf("nil runStore List = %v, want nil", got)
	}
	if got := rs.Recent("j", 1); got != nil {
		t.Fatalf("nil runStore Recent = %v, want nil", got)
	}
	if _, err := rs.Get("j", "r"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nil runStore Get err = %v, want fs.ErrNotExist", err)
	}
	// Append / DeleteJob / RecentSessionIDs must no-op, not panic.
	rs.Append(&CronRun{JobID: "j", RunID: "r"})
	rs.DeleteJob("j")
	if got := rs.RecentSessionIDs("j", 1); got != nil {
		t.Fatalf("nil runStore RecentSessionIDs = %v, want nil", got)
	}
}
