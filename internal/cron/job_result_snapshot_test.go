package cron

import (
	"testing"
	"time"
)

// TestJobResultSnapshotRestore pins the contract that jobResultSnapshot.restore
// returns every field captured at snapshot time back to the target Job. The
// recordTerminalResult rollback path relies on this round-trip when
// persistJobsLocked fails — drift between the field set captured here and the
// fields mutated under s.mu would silently leak partially-updated state into
// dashboard reads. R247-CR-14 (#586).
func TestJobResultSnapshotRestore(t *testing.T) {
	t0 := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	original := jobResultSnapshot{
		LastRunAt:      t0,
		LastResult:     "prior-result",
		LastError:      "prior-err",
		LastErrorClass: ErrClassSessionError,
		LastSessionID:  "sess-prior",
		Counters:       JobRunCounters{Total: 6, Succeeded: 3, Failed: 1, Canceled: 0, Skipped: 2},
	}

	j := &Job{
		ID:             "abcd1234abcd1234",
		LastRunAt:      time.Date(2026, 5, 27, 13, 0, 0, 0, time.UTC),
		LastResult:     "mutated-result",
		LastError:      "mutated-err",
		LastErrorClass: ErrClassWorkDirUnreachable,
		LastSessionID:  "sess-mutated",
		RunCounters:    JobRunCounters{Total: 99, Succeeded: 99, Failed: 99, Canceled: 99, Skipped: 99},
	}
	original.restore(j)

	if !j.LastRunAt.Equal(t0) {
		t.Errorf("LastRunAt = %v, want %v", j.LastRunAt, t0)
	}
	if j.LastResult != "prior-result" {
		t.Errorf("LastResult = %q, want %q", j.LastResult, "prior-result")
	}
	if j.LastError != "prior-err" {
		t.Errorf("LastError = %q, want %q", j.LastError, "prior-err")
	}
	if j.LastErrorClass != ErrClassSessionError {
		t.Errorf("LastErrorClass = %v, want %v", j.LastErrorClass, ErrClassSessionError)
	}
	if j.LastSessionID != "sess-prior" {
		t.Errorf("LastSessionID = %q, want %q", j.LastSessionID, "sess-prior")
	}
	want := JobRunCounters{Total: 6, Succeeded: 3, Failed: 1, Canceled: 0, Skipped: 2}
	if j.RunCounters != want {
		t.Errorf("RunCounters = %+v, want %+v", j.RunCounters, want)
	}
}

// TestJobSnapshotResultStateRoundTrip pins R249-ARCH-22 (#986): the capture
// helper Job.snapshotResultState and jobResultSnapshot.restore must be exact
// inverses over the runtime-mutable result-state cluster. If a future change
// adds a Last* / RunCounters-style state field to either snapshotResultState
// or restore without the other, a snapshot→mutate→restore cycle would no
// longer return the Job to its pre-mutation state and the recordTerminalResult
// rollback path would silently leak partial state. Seeding distinct pre/post
// values on every field and asserting full equality after the round-trip
// catches that drift.
func TestJobSnapshotResultStateRoundTrip(t *testing.T) {
	pre := &Job{
		ID:             "abcd1234abcd1234",
		LastRunAt:      time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC),
		LastResult:     "pre-result",
		LastError:      "pre-err",
		LastErrorClass: ErrClassDeadlineExceeded,
		LastSessionID:  "sess-pre",
		RunCounters:    JobRunCounters{Total: 10, Succeeded: 7, Failed: 2, TimedOut: 1, Skipped: 0, Canceled: 0},
	}

	snap := pre.snapshotResultState()

	// Mutate every state field to a distinct value (simulating a terminal
	// result write) so a forgotten restore field would survive.
	pre.LastRunAt = time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	pre.LastResult = "post-result"
	pre.LastError = "post-err"
	pre.LastErrorClass = ErrClassCanceled
	pre.LastSessionID = "sess-post"
	pre.RunCounters = JobRunCounters{Total: 11, Succeeded: 7, Failed: 2, TimedOut: 1, Skipped: 0, Canceled: 1}

	snap.restore(pre)

	if !pre.LastRunAt.Equal(time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("LastRunAt not restored: got %v", pre.LastRunAt)
	}
	if pre.LastResult != "pre-result" {
		t.Errorf("LastResult not restored: got %q", pre.LastResult)
	}
	if pre.LastError != "pre-err" {
		t.Errorf("LastError not restored: got %q", pre.LastError)
	}
	if pre.LastErrorClass != ErrClassDeadlineExceeded {
		t.Errorf("LastErrorClass not restored: got %v", pre.LastErrorClass)
	}
	if pre.LastSessionID != "sess-pre" {
		t.Errorf("LastSessionID not restored: got %q", pre.LastSessionID)
	}
	wantCounters := JobRunCounters{Total: 10, Succeeded: 7, Failed: 2, TimedOut: 1, Skipped: 0, Canceled: 0}
	if pre.RunCounters != wantCounters {
		t.Errorf("RunCounters not restored: got %+v want %+v", pre.RunCounters, wantCounters)
	}
}
