package cron

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunStore_Append_PostMarshalShrinkRecomputesResultBytes pins
// R20260610-085718-LB-11 (#2016): on the post-marshal over-cap retry path,
// Append truncates Result/Prompt/ErrorMsg to maxRetryFieldRunes but must also
// recompute ResultBytes so the persisted record is self-consistent.
//
// The run.go contract states ResultBytes is the STORED byte count — what the
// dashboard run-detail API renders. Before the fix, the value copied from the
// pre-truncation CronRun (scheduler_finish.go sets it before truncation) was
// left untouched in the shrink branch, so disk would carry result_bytes≈4096
// while result was only ~270 bytes.
//
// To exercise the POST-MARSHAL branch (not the cheap len()-sum preflight) we
// fill Result with `<` runes: json.Marshal HTML-escapes each as `<`, a
// 6× byte expansion. The raw byte sum stays under (cap - fixedFieldsHeadroom)
// so preflight is skipped, but the encoded payload overshoots the cap and
// trips the post-marshal shrink retry.
func TestRunStore_Append_PostMarshalShrinkRecomputesResultBytes(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	const tightCap = int64(8 * 1024)
	s := newRunStore(storePath, 10, time.Hour, tightCap)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore must succeed; got disabled")
	}

	// 2000 raw bytes ('<') → sum well under (tightCap-1024)=7168 so preflight
	// is skipped; HTML-escaped to ~12000 bytes it blows past tightCap=8192,
	// forcing the post-marshal shrink branch.
	rawResult := strings.Repeat("<", 2000)
	run := &CronRun{
		JobID:     "0123456789abcdef",
		RunID:     "fedcba9876543210",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
		Result:    rawResult,
		// Stale pre-truncation byte count, as scheduler_finish.go would have
		// stamped it before the runStore shrink. The fix must overwrite this.
		ResultBytes: len(rawResult),
	}
	s.Append(run)

	got, err := s.Get(run.JobID, run.RunID)
	if err != nil {
		t.Fatalf("Get after post-marshal shrink Append: err = %v; want truncated record stored", err)
	}

	// Sanity: the shrink path actually fired (Result is much smaller than raw).
	if len(got.Result) >= len(rawResult) {
		t.Fatalf("expected post-marshal shrink to truncate Result; got %d bytes (raw was %d) — test no longer exercises the shrink branch",
			len(got.Result), len(rawResult))
	}

	// Core invariant (#2016): persisted ResultBytes must equal the stored
	// Result byte length, not the stale pre-truncation value.
	if got.ResultBytes != len(got.Result) {
		t.Errorf("ResultBytes=%d, want len(Result)=%d — shrink branch must recompute ResultBytes",
			got.ResultBytes, len(got.Result))
	}
	if got.ResultBytes == len(rawResult) {
		t.Errorf("ResultBytes still equals stale pre-truncation length %d; recompute missing",
			len(rawResult))
	}
}
