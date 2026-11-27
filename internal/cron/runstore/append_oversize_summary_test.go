package runstore

import (
	"strings"
	"testing"
	"time"
)

// TestAppend_PreflightOverCap_TruncatesWithoutFirstMarshal covers the preflight
// over-cap entry point (#1111): when Result+Prompt+ErrorMsg alone overshoot the
// cap, Append skips the doomed first marshal and truncates directly. The record
// on disk is truncated, and the cache row (List's source) is that record's
// summary (#1079).
func TestAppend_PreflightOverCap_TruncatesWithoutFirstMarshal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.maxRunBytes = 2048

	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	// Well past maxRunBytes-fixedFieldsHeadroom, so preflightOverCap is true.
	run.Result = strings.Repeat("Y", 8192)

	s.Append(run)

	got := s.Recent(jobID, 5)
	if len(got) != 1 {
		t.Fatalf("Recent len=%d want 1; the preflight path must still land a record", len(got))
	}
	disk, err := s.Get(jobID, run.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(disk.Result) >= len(run.Result) {
		t.Fatalf("disk Result length=%d not truncated on the preflight path", len(disk.Result))
	}
	if disk.ResultBytes != len(disk.Result) {
		t.Errorf("ResultBytes=%d but stored Result is %d bytes (#2016)", disk.ResultBytes, len(disk.Result))
	}
	if want := disk.summary(); got[0] != want {
		t.Errorf("cache row disagrees with the record on disk:\n cache = %+v\n disk  = %+v", got[0], want)
	}
}
