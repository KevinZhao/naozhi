package cron

import (
	"strings"
	"testing"
	"time"
)

// TestAppend_HistoryDropTotal_BumpsOnUnrecoverableOversize pins R249-CR-21
// (#964): when even the truncated retry payload still exceeds maxRunBytes the
// run record is dropped, and Append must now bump historyDropTotal so the loss
// is observable as a metric (not just a slog.Warn). Drives the drop with a
// maxRunBytes set below the fixed-metadata floor so no payload can fit.
func TestAppend_HistoryDropTotal_BumpsOnUnrecoverableOversize(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	// Smaller than the JSON of even an empty CronRun's required fields, so the
	// truncated retry cannot land either — forces the drop branch.
	s.maxRunBytes = 16

	if got := s.HistoryDropTotal(); got != 0 {
		t.Fatalf("HistoryDropTotal before append = %d want 0", got)
	}

	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	run.Result = strings.Repeat("X", 4096)
	s.Append(run)

	if got := s.HistoryDropTotal(); got != 1 {
		t.Fatalf("HistoryDropTotal after unrecoverable oversize = %d want 1", got)
	}
	// Nothing should have landed on disk.
	if got := s.Recent(jobID, 5); len(got) != 0 {
		t.Fatalf("Recent len=%d want 0 (record should have been dropped)", len(got))
	}

	// A second drop accumulates (monotonic counter).
	run2 := makeRun(mustGenerateID(), time.Now())
	run2.Result = strings.Repeat("Y", 4096)
	s.Append(run2)
	if got := s.HistoryDropTotal(); got != 2 {
		t.Fatalf("HistoryDropTotal after second drop = %d want 2", got)
	}
}

// TestAppend_HistoryDropTotal_NoBumpOnSuccess guards the negative: a normal
// (or truncate-and-recover) Append must NOT touch the drop counter, so the
// metric stays a clean signal of actual losses.
func TestAppend_HistoryDropTotal_NoBumpOnSuccess(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	// Large enough that the truncated retry record lands fine.
	s.maxRunBytes = 2048

	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	run.Result = strings.Repeat("Z", 4096) // triggers truncate-retry, which succeeds
	s.Append(run)

	if got := s.HistoryDropTotal(); got != 0 {
		t.Fatalf("HistoryDropTotal after recoverable oversize = %d want 0 (record landed)", got)
	}
	if got := s.Recent(jobID, 5); len(got) != 1 {
		t.Fatalf("Recent len=%d want 1 (truncated record should have landed)", len(got))
	}
}
