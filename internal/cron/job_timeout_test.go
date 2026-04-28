package cron

import (
	"testing"
	"time"
)

func TestComputeJobTimeout(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		cap      time.Duration
		want     time.Duration
	}{
		{
			name:     "hourly job under 1h cap scales to 48m",
			schedule: "@every 1h",
			cap:      time.Hour,
			want:     time.Duration(float64(time.Hour) * jobTimeoutRatio),
		},
		{
			name:     "6h schedule with 1h cap clamps to cap",
			schedule: "@every 6h",
			cap:      time.Hour,
			want:     time.Hour,
		},
		{
			name:     "6h schedule with 8h cap scales to 4h48m",
			schedule: "@every 6h",
			cap:      8 * time.Hour,
			want:     time.Duration(float64(6*time.Hour) * jobTimeoutRatio),
		},
		{
			name:     "10m schedule scales to 8m",
			schedule: "@every 10m",
			cap:      time.Hour,
			want:     time.Duration(float64(10*time.Minute) * jobTimeoutRatio),
		},
		{
			name:     "5m schedule scales to 4m (above floor)",
			schedule: "@every 5m",
			cap:      time.Hour,
			want:     time.Duration(float64(5*time.Minute) * jobTimeoutRatio),
		},
		{
			// minCronInterval blocks this at registration, but computeJobTimeout
			// still needs a defensive floor for jobs loaded from a hand-edited
			// store file or a schedule that evaluates below 3m worth of budget.
			name:     "sub-floor schedule floors at minJobTimeout",
			schedule: "@every 1m",
			cap:      time.Hour,
			want:     minJobTimeout,
		},
		{
			name:     "unparseable schedule falls back to cap",
			schedule: "not a cron expression",
			cap:      time.Hour,
			want:     time.Hour,
		},
		{
			name:     "daily cron expression scales to 19h12m under 24h cap",
			schedule: "0 9 * * *",
			cap:      24 * time.Hour,
			want:     time.Duration(float64(24*time.Hour) * jobTimeoutRatio),
		},
		{
			// Defensive contract: caller-supplied cap below the 3m floor
			// still wins. Production never hits this (default cap is 5m,
			// clamped ≥ 5m in applyDefaults), but the helper is called
			// directly from tests and must not let a pathological tiny cap
			// be overridden upward.
			name:     "cap below floor wins over floor",
			schedule: "@every 10m",
			cap:      30 * time.Second,
			want:     30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeJobTimeout(tc.schedule, tc.cap)
			if got != tc.want {
				t.Fatalf("computeJobTimeout(%q, %v) = %v, want %v",
					tc.schedule, tc.cap, got, tc.want)
			}
		})
	}
}
