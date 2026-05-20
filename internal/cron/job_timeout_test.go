package cron

import (
	"testing"
	"time"
)

// TestComputeJobTimeout verifies the per-run deadline is the configured
// maxCap regardless of schedule. Long-running tasks that overshoot their
// schedule period are not killed; the next scheduled tick is dropped by
// robfig/cron's SkipIfStillRunning chain wrapper.
func TestComputeJobTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		schedule string
		cap      time.Duration
		want     time.Duration
	}{
		{
			name:     "hourly under 8h cap returns cap",
			schedule: "@every 1h",
			cap:      8 * time.Hour,
			want:     8 * time.Hour,
		},
		{
			name:     "6h schedule with 1h cap returns cap",
			schedule: "@every 6h",
			cap:      time.Hour,
			want:     time.Hour,
		},
		{
			name:     "10m schedule with 1h cap returns cap",
			schedule: "@every 10m",
			cap:      time.Hour,
			want:     time.Hour,
		},
		{
			name:     "unparseable schedule returns cap",
			schedule: "not a cron expression",
			cap:      time.Hour,
			want:     time.Hour,
		},
		{
			name:     "daily cron expression returns cap",
			schedule: "0 9 * * *",
			cap:      24 * time.Hour,
			want:     24 * time.Hour,
		},
		{
			name:     "tiny cap is honoured (operator hard ceiling)",
			schedule: "@every 10m",
			cap:      30 * time.Second,
			want:     30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := computeJobTimeout(tc.schedule, tc.cap)
			if got != tc.want {
				t.Fatalf("computeJobTimeout(%q, %v) = %v, want %v",
					tc.schedule, tc.cap, got, tc.want)
			}
		})
	}
}
