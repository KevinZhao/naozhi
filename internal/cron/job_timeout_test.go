package cron

import (
	"testing"
	"time"
)

// TestComputeJobTimeout verifies the per-run deadline is the configured
// maxCap. Long-running tasks that overshoot their schedule period are not
// killed; the next scheduled tick is dropped by robfig/cron's
// SkipIfStillRunning chain wrapper.
func TestComputeJobTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cap  time.Duration
		want time.Duration
	}{
		{name: "8h cap", cap: 8 * time.Hour, want: 8 * time.Hour},
		{name: "1h cap", cap: time.Hour, want: time.Hour},
		{name: "24h cap", cap: 24 * time.Hour, want: 24 * time.Hour},
		{name: "tiny cap is honoured (operator hard ceiling)", cap: 30 * time.Second, want: 30 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := computeJobTimeout(tc.cap)
			if got != tc.want {
				t.Fatalf("computeJobTimeout(%v) = %v, want %v",
					tc.cap, got, tc.want)
			}
		})
	}
}
