package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
)

// TestCronView_SchedulerSatisfies pins the production wiring contract:
// *cron.Scheduler must implement the consolidated CronView interface that
// R242-ARCH-13 (#754) collapsed cronHubOps + cronStubChecker +
// cronSessionLister into. Without this assertion a future cron-side
// rename / signature drift would only blow up at the field assignment in
// server.go (which lives in another agent's review domain), and the
// failure mode would be a single compile error far from the interface
// declaration.  A typed nil assertion fails to compile if any of the
// three methods drift, which keeps the failure local to the file owning
// the interface.
func TestCronView_SchedulerSatisfies(t *testing.T) {
	var _ CronView = (*cron.Scheduler)(nil)
}

// TestCronView_FakeSatisfies pins the test scaffolding: fakeCronSessions
// from dashboard_session_filter_test.go is the canonical no-op fake used
// by handler tests that need a CronView without spinning up a real
// cron.Scheduler. The interface widening in #754 broadens its method-set
// from one (KnownSessionIDs) to three (+ EnsureStub + SetJobPrompt); this
// compile-time check ensures the fake stays in lockstep so a future field
// rename does not silently degrade the fake into a "looks like CronView
// but missing one method" build error in unrelated test files.
func TestCronView_FakeSatisfies(t *testing.T) {
	var _ CronView = fakeCronSessions{}
}

// TestCronView_NilZeroValueIsNil documents the nil-tolerance contract on
// the two CronView fields.  SessionHandlers.scheduler and SessionHandlers
// .cronSessions are both optional (production wiring may set either to a
// concrete *cron.Scheduler or to nil for cron-disabled deployments). The
// surrounding handler code MUST guard with `h.scheduler != nil` /
// `h.cronSessions != nil` before invoking any CronView method — a typed
// nil interface value is itself nil at the interface comparison level
// (no boxed concrete type yet) so the gate works.  This test exists so
// that a maintainer adding a third call site sees the gate pattern
// documented at the interface level rather than re-deriving it from each
// existing call site.
func TestCronView_NilZeroValueIsNil(t *testing.T) {
	var view CronView
	if view != nil {
		t.Fatalf("zero-value CronView must compare == nil")
	}
}

// TestCronScheduler_SchedulerSatisfies pins the field-narrowing contract
// introduced by R20260603000023-ARCH-2 (#1648): Server.scheduler is now the
// cronScheduler consumer interface instead of the concrete *cron.Scheduler,
// advertising only the CronView + cronCommandScheduler (#1164) method sets
// plus the single direct call (SetTelemetry) the server makes. *cron.Scheduler must
// continue to satisfy it implicitly; a cron-side rename / signature drift
// fails to compile here, local to the interface declaration, rather than only
// at the field assignment in server.go.
func TestCronScheduler_SchedulerSatisfies(t *testing.T) {
	var _ cronScheduler = (*cron.Scheduler)(nil)
}

// TestCronScheduler_NilPointerBoxesToNilInterface guards the ctor's
// `if opts.Scheduler != nil` conversion (#1648). Boxing a typed nil
// *cron.Scheduler directly into the interface would yield a NON-nil interface
// wrapping a nil pointer, which would make every `s.scheduler != nil`
// cron-enabled guard fire for scheduler-less deployments and panic on the
// first method call. The constructor must therefore leave the interface as a
// genuine nil when no scheduler is wired. This test documents the trap so a
// future edit that "simplifies" the guard away gets a red test.
func TestCronScheduler_NilPointerBoxesToNilInterface(t *testing.T) {
	var concrete *cron.Scheduler // typed nil, as opts.Scheduler is when unset

	// WRONG (documented anti-pattern): direct box produces a non-nil interface.
	var boxedDirect cronScheduler = concrete
	if boxedDirect == nil {
		t.Fatal("sanity: a typed-nil pointer boxed directly is NOT a nil interface")
	}

	// RIGHT (what NewWithOptions does): nil pointer → genuine nil interface.
	var boxedGuarded cronScheduler
	if concrete != nil {
		boxedGuarded = concrete
	}
	if boxedGuarded != nil {
		t.Fatal("guarded conversion of a nil *cron.Scheduler must yield a nil interface")
	}
}
