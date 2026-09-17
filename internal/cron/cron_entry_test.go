package cron

import (
	"reflect"
	"strings"
	"testing"
	"time"

	robfigcron "github.com/robfig/cron/v3"
)

// TestRegisterJob_KeepsTheSchedulerChain is the regression the switch from
// AddFunc to Schedule could have introduced silently. NewScheduler installs
// Recover + SkipIfStillRunning as a chain; AddFunc applied it, and Schedule has
// to as well — robfig builds both entry kinds with WrappedJob: c.chain.Then(cmd)
// (cron.go:165). If the chain were lost, a panicking cron job would take the
// process down and an overrunning job would double-fire, and neither shows up
// until tick time, so no existing test would have caught it.
//
// The assertion compares code pointers because that is what "a wrapper was
// applied" means here: FuncJob is a func type, so the values are uncomparable,
// and with an empty chain robfig's Then returns the job unchanged — identical
// pointers. So a differing pointer is exactly "the chain wrapped it".
func TestRegisterJob_KeepsTheSchedulerChain(t *testing.T) {
	t.Parallel()
	// Two cases, and the unchained one is what gives the assertion teeth: robfig
	// exposes no way to register while skipping the chain, so the mutation
	// "commit without the chain" cannot be written. Asserting that an unchained
	// Cron DOES produce identical pointers proves the comparison distinguishes
	// the two rather than always passing.
	cases := []struct {
		name        string
		chained     bool
		wantWrapped bool
	}{
		{"scheduler chain installed", true, true},
		{"no chain (control: proves the comparison can fail)", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []robfigcron.Option{robfigcron.WithParser(cronParser)}
			if tc.chained {
				opts = append(opts, robfigcron.WithChain(robfigcron.Recover(robfigcron.DiscardLogger)))
			}
			s := &Scheduler{jobs: make(map[string]*Job), cron: robfigcron.New(opts...)}

			j := &Job{ID: "0123456789abcdef", Schedule: "@every 1h"}
			if err := s.registerJob(j); err != nil {
				t.Fatalf("registerJob: %v", err)
			}
			t.Cleanup(func() { s.cron.Remove(j.entryID) })

			entry := s.cron.Entry(j.entryID)
			if entry.ID != j.entryID {
				t.Fatalf("entry %d not found after registerJob", j.entryID)
			}
			if entry.Job == nil || entry.WrappedJob == nil {
				t.Fatal("entry has no job; nothing was registered")
			}
			wrapped := reflect.ValueOf(entry.WrappedJob).Pointer() != reflect.ValueOf(entry.Job).Pointer()
			if wrapped != tc.wantWrapped {
				if tc.wantWrapped {
					t.Error("WrappedJob is the bare tick callback: the scheduler chain was not applied, " +
						"so Recover and SkipIfStillRunning are not in effect for this entry")
				} else {
					t.Error("an unchained Cron produced a wrapped job; the pointer comparison does not " +
						"distinguish chained from unchained, so the chained case above proves nothing")
				}
			}
		})
	}
}

// TestRegisterJob_ParseFailureRegistersNothing: the parse now happens before the
// entry is created, so a bad schedule must leave robfig untouched. Under the old
// shape AddFunc parsed and registered in one call, so this ordering was its
// property rather than ours — worth pinning now that we own it.
func TestRegisterJob_ParseFailureRegistersNothing(t *testing.T) {
	t.Parallel()
	s := &Scheduler{jobs: make(map[string]*Job), cron: robfigcron.New(robfigcron.WithParser(cronParser))}

	before := len(s.cron.Entries())
	j := &Job{ID: "0123456789abcdef", Schedule: "not a schedule"}
	err := s.registerJob(j)
	if err == nil {
		t.Fatal("registerJob accepted an unparseable schedule")
	}
	// Callers surface this text (UpdateJob's rollback logs it), so the prefix is
	// part of the contract.
	if !strings.HasPrefix(err.Error(), "register cron: ") {
		t.Errorf("err = %q, want the 'register cron: ' prefix callers already log", err)
	}
	if j.entryID != 0 {
		t.Errorf("entryID = %d after a failed registration, want 0", j.entryID)
	}
	if j.cachedSched != nil || j.cachedPeriod != 0 {
		t.Errorf("cache written on a failed registration: sched=%v period=%v", j.cachedSched, j.cachedPeriod)
	}
	if after := len(s.cron.Entries()); after != before {
		t.Errorf("robfig entry count %d → %d; a rejected schedule must register nothing", before, after)
	}
}

// TestPlanCronEntry_IsPureAndMatchesTheCache pins that the cached period comes
// from the same parsed schedule that gets registered. The old shape read the
// schedule back out of robfig to compute this, which is the round trip removed;
// if the two ever diverged, jitter would be computed against a schedule the
// entry does not have.
func TestPlanCronEntry_IsPureAndMatchesTheCache(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	p, err := planCronEntry("0123456789abcdef", "*/5 * * * *", now)
	if err != nil {
		t.Fatalf("planCronEntry: %v", err)
	}
	if p.sched == nil {
		t.Fatal("plan carries no parsed schedule")
	}
	if p.period != 5*time.Minute {
		t.Errorf("period = %v, want 5m", p.period)
	}

	s := &Scheduler{jobs: make(map[string]*Job), cron: robfigcron.New(robfigcron.WithParser(cronParser))}
	j := &Job{ID: p.jobID, Schedule: "*/5 * * * *"}
	if err := s.registerJob(j); err != nil {
		t.Fatalf("registerJob: %v", err)
	}
	t.Cleanup(func() { s.cron.Remove(j.entryID) })

	// The registered entry's schedule must produce the same next fire time as the
	// plan's — same object in practice, and this asserts it behaviourally rather
	// than by identity.
	if got, want := s.cron.Entry(j.entryID).Schedule.Next(now), p.sched.Next(now); !got.Equal(want) {
		t.Errorf("registered schedule Next = %v, plan Next = %v — the cache describes a different schedule than the entry", got, want)
	}
	if j.cachedPeriod != p.period {
		t.Errorf("cachedPeriod = %v, plan period = %v", j.cachedPeriod, p.period)
	}
}
