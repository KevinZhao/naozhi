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
