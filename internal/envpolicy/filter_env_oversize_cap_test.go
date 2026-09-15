package envpolicy

import (
	"strings"
	"testing"
)

// TestShimEnvDrops_OversizeReportCap pins [R20260603-SEC-5]: when many oversized
// env entries are present, the REPORT is capped at maxShimEnvOversizeReports (5)
// rather than emitting exactly one (the old sync.Once behavior that let a benign
// oversized entry mask later attacker-injected ones). Every oversized entry is
// still dropped — the cap is on reporting, never on rejection.
//
// Not parallel: it resets the process-global oversize counter, shared with other
// tests.
func TestShimEnvDrops_OversizeReportCap(t *testing.T) {
	// Reset the shared counter so this test sees a clean budget regardless of
	// other tests that may have incremented it earlier in the run.
	shimEnvOversizeReports.Store(0)

	big := strings.Repeat("x", maxShimEnvEntryBytes+1)
	const oversizedCount = 12
	input := make([]string, 0, oversizedCount+1)
	input = append(input, "HOME=/home/user") // one benign, allowed entry
	for i := 0; i < oversizedCount; i++ {
		input = append(input, "BIG_VAR="+big) // oversized — must be dropped
	}

	// Only the benign HOME entry survives; every oversized entry is dropped.
	if got := FilterShimEnv(input); len(got) != 1 || got[0] != "HOME=/home/user" {
		t.Fatalf("expected only HOME to survive, got %v", got)
	}

	drops := ShimEnvDrops(input)
	if len(drops) != maxShimEnvOversizeReports {
		t.Fatalf("expected %d oversize reports (capped), got %d", maxShimEnvOversizeReports, len(drops))
	}
	// The last one inside the budget says so, so an operator reading the log
	// knows more were dropped than reported.
	if last := drops[len(drops)-1].Reason; !strings.Contains(last, "further oversized reports suppressed") {
		t.Errorf("the report at the cap must announce the suppression, got %q", last)
	}
	// Key prefix only: the value may be a secret.
	for _, d := range drops {
		if strings.Contains(d.Reason, "xxxx") || strings.Contains(d.Key, "xxxx") {
			t.Errorf("oversize report leaked the value: %+v", d)
		}
	}
}
