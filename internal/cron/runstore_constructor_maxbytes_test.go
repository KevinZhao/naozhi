package cron

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunStore_NewRunStore_MaxBytesParity pins R241-ARCH-8 (#512):
// newRunStore now accepts an optional maxBytesOpt that overrides
// MaxRunRecordBytes, bringing constructor signature in parity with
// keepCount / keepWindow which were already tunable. The variadic
// keeps existing 3-arg call sites compatible — a missing or
// non-positive value falls back to the MaxRunRecordBytes default so
// the production caller in scheduler.go observes no behaviour change.
//
// Anchor: R241-ARCH-8. Test prevents regression where the constructor
// hard-codes MaxRunRecordBytes regardless of the optional override.
func TestRunStore_NewRunStore_MaxBytesParity(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")

	// Default: no override → uses MaxRunRecordBytes.
	sDefault := newRunStore(storePath, 10, time.Hour)
	if sDefault == nil || sDefault.disabled {
		t.Fatalf("default newRunStore must succeed; got disabled")
	}
	if sDefault.maxRunBytes != int64(MaxRunRecordBytes) {
		t.Errorf("default maxRunBytes = %d; want MaxRunRecordBytes=%d",
			sDefault.maxRunBytes, MaxRunRecordBytes)
	}

	// Explicit override → used as-is.
	const tinyCap = int64(1024)
	tmp2 := t.TempDir()
	storePath2 := filepath.Join(tmp2, "cron_jobs.json")
	sCustom := newRunStore(storePath2, 10, time.Hour, tinyCap)
	if sCustom == nil || sCustom.disabled {
		t.Fatalf("custom newRunStore must succeed; got disabled")
	}
	if sCustom.maxRunBytes != tinyCap {
		t.Errorf("custom maxRunBytes = %d; want %d", sCustom.maxRunBytes, tinyCap)
	}

	// Zero / negative override → fall back to MaxRunRecordBytes
	// (mirrors keepCount / keepWindow zero-handling).
	tmp3 := t.TempDir()
	storePath3 := filepath.Join(tmp3, "cron_jobs.json")
	sZero := newRunStore(storePath3, 10, time.Hour, 0)
	if sZero.maxRunBytes != int64(MaxRunRecordBytes) {
		t.Errorf("zero-override maxRunBytes = %d; want MaxRunRecordBytes=%d",
			sZero.maxRunBytes, MaxRunRecordBytes)
	}

	tmp4 := t.TempDir()
	storePath4 := filepath.Join(tmp4, "cron_jobs.json")
	sNeg := newRunStore(storePath4, 10, time.Hour, -100)
	if sNeg.maxRunBytes != int64(MaxRunRecordBytes) {
		t.Errorf("negative-override maxRunBytes = %d; want MaxRunRecordBytes=%d",
			sNeg.maxRunBytes, MaxRunRecordBytes)
	}
}

// TestRunStore_NewRunStore_MaxBytesEnforced confirms the override
// actually takes effect on the Append path — Append truncates and/or
// drops records that exceed the configured cap. Pinning the cap
// override end-to-end prevents a regression where the constructor
// stores maxBytesOpt but a downstream code path reads the global
// MaxRunRecordBytes constant directly. R241-ARCH-8 (#512).
func TestRunStore_NewRunStore_MaxBytesEnforced(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	const tinyCap = int64(512)
	s := newRunStore(storePath, 10, time.Hour, tinyCap)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore must succeed; got disabled")
	}

	// Build a CronRun whose marshalled JSON comfortably exceeds tinyCap.
	// Result/Prompt fields are the dominant size contributors; each
	// truncates to maxRetryFieldRunes on the over-cap retry path.
	run := &CronRun{
		JobID:     "0123456789abcdef",
		RunID:     "fedcba9876543210",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
		Result:    strings.Repeat("X", 4096),
		Prompt:    strings.Repeat("Y", 4096),
	}
	s.Append(run)

	// After Append, the on-disk record must have been truncated under
	// tinyCap (or dropped). Either way, Get returns a CronRun with
	// Result fields shrunk to maxRetryFieldRunes runes (256).
	got, err := s.Get(run.JobID, run.RunID)
	if err != nil {
		// over-cap retry can drop the record entirely; that is also
		// honoring the cap. Either outcome demonstrates the cap is
		// active. Test guard catches the bug where the cap is ignored
		// (the original ~8KB record would land verbatim).
		return
	}
	if len(got.Result) > maxRetryFieldRunes*4 { // 4 byte/rune upper bound
		t.Errorf("Result length %d post-Append should be truncated to ~%d runes (cap=%d)",
			len(got.Result), maxRetryFieldRunes, tinyCap)
	}
}
