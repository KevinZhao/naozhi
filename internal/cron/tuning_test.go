package cron

import (
	"testing"
	"time"
)

// TestTuningDefaults pins the centralised cron tuning constants (R249-CR-16,
// #959) so a move/edit of tuning.go that silently changes a default trips a
// test rather than shipping. The values are also cross-referenced by the
// operator table at the top of tuning.go.
func TestTuningDefaults(t *testing.T) {
	t.Parallel()

	if defaultCronSlowThreshold != 30*time.Second {
		t.Errorf("defaultCronSlowThreshold = %v, want 30s", defaultCronSlowThreshold)
	}
	if spawnElapsedWarnRatio != 0.5 {
		t.Errorf("spawnElapsedWarnRatio = %v, want 0.5", spawnElapsedWarnRatio)
	}
	if minSendBudget != 30*time.Second {
		t.Errorf("minSendBudget = %v, want 30s", minSendBudget)
	}
	if cronNotifyTimeout != 30*time.Second {
		t.Errorf("cronNotifyTimeout = %v, want 30s", cronNotifyTimeout)
	}

	// R249-ARCH-23 (#987): cronNotifyTimeout is the OUTER per-target ceiling;
	// it must stay aligned with the inner retry/chunk budgets so the composite
	// flush cannot silently outlive the per-target deadline contract.
	if cronNotifyMaxChunks <= 0 {
		t.Errorf("cronNotifyMaxChunks = %d, want > 0", cronNotifyMaxChunks)
	}
}
