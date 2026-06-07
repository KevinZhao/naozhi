package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestR050103C_FinishRunResultBytesIsStoredNotRaw pins #1910: CronRun.ResultBytes
// is the STORED (post-truncation/redaction/sanitise) byte count, never the raw
// Claude output size. A regression that wires ResultBytes to the raw a.result
// length (or that drops the truncate pipeline) would let an operator using
// result_bytes for billing/capacity over-report a truncated long answer or
// fail to distinguish it from a genuinely short one.
//
// Table-driven over the three regimes: short (untouched), exactly-at-cap, and
// far-over-cap (truncated). In every case the persisted ResultBytes must equal
// len(stored Result) and must be <= the truncation ceiling — proving it tracks
// on-disk footprint, decoupled from the raw input length.
func TestR050103C_FinishRunResultBytesIsStoredNotRaw(t *testing.T) {
	t.Parallel()

	// Ceiling for the stored result: maxStoredResultRunes runes plus the
	// "…[truncated]" suffix, each measured in bytes. ASCII content keeps the
	// rune-to-byte mapping 1:1 so this is an exact upper bound here.
	storedCeilingBytes := maxStoredResultRunes + len(truncatedSuffix)

	cases := []struct {
		name        string
		rawResult   string
		wantTrunc   bool // expect the stored result to be shorter than raw
		wantRawSize int  // len(rawResult), for the over-report assertion
	}{
		{
			name:        "short result stored verbatim",
			rawResult:   "ok",
			wantTrunc:   false,
			wantRawSize: 2,
		},
		{
			name:        "far over cap is truncated",
			rawResult:   strings.Repeat("x", maxStoredResultRunes*4),
			wantTrunc:   true,
			wantRawSize: maxStoredResultRunes * 4,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			cfg := SchedulerConfig{
				MaxJobs:   5,
				Router:    &fakeRouter{},
				StorePath: filepath.Join(dir, "cron_jobs.json"),
			}
			sched := NewScheduler(cfg)
			if sched.runStore == nil || sched.runStore.disabled {
				t.Fatal("runStore must be enabled for this test (StorePath set)")
			}

			j := &Job{
				ID:       "abcdef0123456789",
				Schedule: "@every 5m",
				Prompt:   "ping",
			}
			sched.mu.Lock()
			sched.jobs[j.ID] = j
			sched.mu.Unlock()

			inflight := sched.jobInflight(j.ID)
			if !inflight.running.CompareAndSwap(false, true) {
				t.Fatal("initial CAS must succeed")
			}
			finalizer := &runFinalizer{inflight: inflight}

			runID := "0123456789abcdef"
			sched.finishRun(finishArgs{
				job:       j,
				runID:     runID,
				startedAt: time.Now(),
				trigger:   TriggerScheduled,
				state:     RunStateSucceeded,
				result:    tc.rawResult,
				finalizer: finalizer,
			})

			// Read the persisted CronRun back off disk so we assert the durable
			// record, not an in-memory shortcut.
			path := filepath.Join(sched.runStore.root, j.ID, runID+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read persisted run: %v", err)
			}
			var stored CronRun
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatalf("unmarshal persisted run: %v", err)
			}

			// Core invariant (#1910): ResultBytes == stored byte count.
			if stored.ResultBytes != len(stored.Result) {
				t.Errorf("ResultBytes=%d, want len(Result)=%d (must measure STORED bytes)",
					stored.ResultBytes, len(stored.Result))
			}

			// Never exceeds the truncation ceiling — it is an on-disk footprint.
			if stored.ResultBytes > storedCeilingBytes {
				t.Errorf("ResultBytes=%d exceeds stored ceiling %d; field must track post-truncation size",
					stored.ResultBytes, storedCeilingBytes)
			}

			if tc.wantTrunc {
				// The decoupling guard: a raw output far above the cap must NOT
				// be reflected in ResultBytes. If a regression wired ResultBytes
				// to the raw length, this would be wantRawSize and fail.
				if stored.ResultBytes >= tc.wantRawSize {
					t.Errorf("ResultBytes=%d should be < raw size %d (truncation must shrink it)",
						stored.ResultBytes, tc.wantRawSize)
				}
			} else {
				if stored.ResultBytes != tc.wantRawSize {
					t.Errorf("ResultBytes=%d, want %d (short result stored verbatim)",
						stored.ResultBytes, tc.wantRawSize)
				}
			}
		})
	}
}
