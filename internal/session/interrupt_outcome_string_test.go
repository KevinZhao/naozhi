package session

import "testing"

// TestInterruptOutcomeString pins the slog tag strings each outcome renders
// to. Cron / router / dashboard code logs `outcome` as a slog attribute and
// downstream parsing (operator grep, dashboard ws filtering) treats these as
// stable identifiers. Changing a tag silently would break those callers, so
// pin every enum value plus the default-branch shape.
func TestInterruptOutcomeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome InterruptOutcome
		want    string
	}{
		{InterruptSent, "sent"},
		{InterruptNoSession, "no_session"},
		{InterruptNoTurn, "no_turn"},
		{InterruptUnsupported, "unsupported"},
		{InterruptError, "error"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			got := tc.outcome.String()
			if got != tc.want {
				t.Fatalf("InterruptOutcome(%d).String() = %q, want %q",
					int(tc.outcome), got, tc.want)
			}
		})
	}

	// Defensive: the default branch reports the integer so an out-of-range
	// value remains diagnosable. The exact format ("unknown(N)") is part
	// of the slog contract — pin it so refactors that swap to e.g. plain
	// strconv.Itoa surface as a test break.
	t.Run("out_of_range_default", func(t *testing.T) {
		t.Parallel()
		got := InterruptOutcome(99).String()
		if got != "unknown(99)" {
			t.Fatalf("InterruptOutcome(99).String() = %q, want %q", got, "unknown(99)")
		}
	})
}
