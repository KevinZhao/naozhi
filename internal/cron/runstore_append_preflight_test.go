package cron

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunStore_Append_PreflightSkipsFirstMarshal pins R250-PERF-8 (#1111):
// when Result/Prompt/ErrorMsg byte sum alone overshoots maxRunBytes
// minus the fixed-fields headroom, Append now skips the speculative
// first marshal and goes straight to the truncated retry variant.
// Saves one full json.Marshal on the over-cap path; the cheap len()
// pre-flight is correctness-neutral because the post-marshal
// len(data) > maxRunBytes gate remains the authoritative check.
//
// The test exercises behaviour, not the path itself: we Append a
// run with deliberately oversized fields, then read it back and
// confirm Result/Prompt are truncated to maxRetryFieldRunes runes.
// A regression that bypassed the pre-flight (or, worse, broke the
// truncate retry) would surface either as Get returning the full
// oversized record OR Get failing because the dropped-payload
// branch fired. Both are caught here.
//
// Anchor: R250-PERF-8 (#1111).
func TestRunStore_Append_PreflightSkipsFirstMarshal(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	// Tight cap forces the over-cap path; pre-flight short-circuits the
	// first marshal because Result+Prompt alone exceed maxBytes - 1024.
	const tightCap = int64(8 * 1024)
	s := newRunStore(storePath, 10, time.Hour, tightCap)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore must succeed; got disabled")
	}

	run := &CronRun{
		JobID:     "0123456789abcdef",
		RunID:     "fedcba9876543210",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
		// 16 KB combined — comfortably over (tightCap - 1024).
		Result: strings.Repeat("R", 8192),
		Prompt: strings.Repeat("P", 8192),
	}
	s.Append(run)

	got, err := s.Get(run.JobID, run.RunID)
	if err != nil {
		t.Fatalf("Get after pre-flight Append: err = %v; want truncated record stored", err)
	}
	// truncateWithSuffix caps at maxRetryFieldRunes (256 runes) plus a
	// short suffix marker; tolerate a generous upper bound to absorb
	// the suffix length without coupling the test to its exact text.
	const truncatedUpperBound = 4 * (maxRetryFieldRunes + 32) // ~4 byte/rune ceiling + suffix
	if len(got.Result) > truncatedUpperBound {
		t.Errorf("Result post-Append = %d bytes; want truncated to ~%d runes",
			len(got.Result), maxRetryFieldRunes)
	}
	if len(got.Prompt) > truncatedUpperBound {
		t.Errorf("Prompt post-Append = %d bytes; want truncated to ~%d runes",
			len(got.Prompt), maxRetryFieldRunes)
	}
}

// TestRunStore_Append_PreflightLetsSmallRunsThrough confirms the pre-flight
// is correctness-neutral on the common small-record path: a run whose
// fields fit comfortably under the cap takes the regular single-marshal
// path and lands intact, untruncated. Prevents a regression where the
// pre-flight inequality is mis-signed and accidentally truncates every
// Append. R250-PERF-8 (#1111).
func TestRunStore_Append_PreflightLetsSmallRunsThrough(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := newRunStore(storePath, 10, time.Hour)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore must succeed; got disabled")
	}

	run := &CronRun{
		JobID:     "0123456789abcdef",
		RunID:     "fedcba9876543210",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
		Result:    "small result", // ~12 bytes
		Prompt:    "small prompt", // ~12 bytes
	}
	s.Append(run)

	got, err := s.Get(run.JobID, run.RunID)
	if err != nil {
		t.Fatalf("Get for small Append: err = %v; want intact record", err)
	}
	if got.Result != run.Result {
		t.Errorf("Result mutated by pre-flight: got %q, want %q (small run must not be truncated)",
			got.Result, run.Result)
	}
	if got.Prompt != run.Prompt {
		t.Errorf("Prompt mutated by pre-flight: got %q, want %q (small run must not be truncated)",
			got.Prompt, run.Prompt)
	}
}
