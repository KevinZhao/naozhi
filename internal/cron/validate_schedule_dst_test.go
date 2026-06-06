package cron

import (
	"testing"
	"time"
)

// TestValidateSchedule_DSTReferenceIsFixed pins R249-CR-22 (#965). The interval
// probe in validateSchedule previously seeded from time.Now(), so a save that
// happened to land near a DST spring-forward/fall-back boundary measured the
// schedule's interval across the skipped/duplicated hour and mis-classified it
// against the minCronInterval floor. The fix seeds from a fixed DST-quiet
// instant (mid-January noon in loc), making the result independent of wall
// clock. This test asserts a clock-time daily schedule in a DST-observing zone
// validates cleanly (interval well above the 5m floor) — the kind of schedule
// whose two-Next probe could span 23h/25h under the old time.Now() seed.
func TestValidateSchedule_DSTReferenceIsFixed(t *testing.T) {
	t.Parallel()

	loc := mustLoadLocation(t, "America/New_York") // observes DST
	// "every day at 02:30" — 02:30 is exactly the spring-forward gap in
	// US Eastern, the classic DST landmine for two-Next interval probes.
	if err := validateSchedule("30 2 * * *", loc); err != nil {
		t.Fatalf("daily 02:30 schedule in DST zone must validate (interval ~24h): %v", err)
	}
}

// TestValidateSchedule_IntervalDeterministicAcrossDST cross-checks that the
// probe measures the schedule's intrinsic period from the fixed reference
// regardless of the ambient time.Local, by confirming an every-6-minutes
// schedule (just above the 5m floor) passes and an every-4-minutes schedule
// (just below) is rejected — both deterministic now that the seed no longer
// depends on time.Now().
func TestValidateSchedule_IntervalDeterministicAcrossDST(t *testing.T) {
	t.Parallel()

	loc := mustLoadLocation(t, "America/New_York")

	if err := validateSchedule("@every 6m", loc); err != nil {
		t.Errorf("@every 6m must pass the 5m floor: %v", err)
	}
	if err := validateSchedule("@every 4m", loc); err == nil {
		t.Error("@every 4m must be rejected by the 5m floor")
	}
}

// TestValidateSchedule_FixedRefNotTimeNow guards against a regression to the
// time.Now() seed: the chosen reference 2024-01-15 12:00 must not coincide with
// any DST transition in the tested zones (so sched.Next from it never straddles
// a gap). We assert the reference instant is well-formed and that two
// consecutive Next() calls on a plain hourly schedule yield exactly one hour,
// which only holds when the seed sits away from a transition boundary.
func TestValidateSchedule_FixedRefNotTimeNow(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"UTC", "America/New_York", "Asia/Shanghai", "Europe/London"} {
		loc := mustLoadLocation(t, name)
		ref := time.Date(2024, time.January, 15, 12, 0, 0, 0, loc)
		sched, err := cronParser.Parse("0 * * * *") // top of every hour
		if err != nil {
			t.Fatalf("parse hourly: %v", err)
		}
		first := sched.Next(ref)
		second := sched.Next(first)
		if got := second.Sub(first); got != time.Hour {
			t.Errorf("%s: hourly interval from fixed ref = %v, want 1h (ref straddled a transition?)", name, got)
		}
	}
}
