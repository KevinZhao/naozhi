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
