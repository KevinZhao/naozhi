package config

import (
	"strconv"
	"testing"
)

// TestLoad_CronAutoPauseAfterFailures: the key is known (no unknown-key
// diag) and its value, negative included, reaches the config unchanged.
func TestLoad_CronAutoPauseAfterFailures(t *testing.T) {
	for _, want := range []int{3, -1} {
		body := "cron:\n  auto_pause_after_failures: " + strconv.Itoa(want) + "\n"
		diags, cfg, err := collectLoadDiags(t, writeCfg(t, body))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		for _, d := range diags {
			t.Errorf("unexpected diag %+v", d)
		}
		if got := cfg.Cron.AutoPauseAfterFailures; got != want {
			t.Errorf("AutoPauseAfterFailures = %d, want %d", got, want)
		}
	}
}
