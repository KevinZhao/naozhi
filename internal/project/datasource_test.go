package project

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestNewDataSource_NilManagerReturnsNilInterface asserts the defense
// against typed-nil interface values documented in
// docs/rfc/key-resolver.md §3.4. Returning `&dataSource{m: nil}` would
// make `data != nil` pass (typed-nil interface) but crash on first
// method call; `return nil` returns untyped nil instead.
func TestNewDataSource_NilManagerReturnsNilInterface(t *testing.T) {
	t.Parallel()
	if got := NewDataSource(nil); got != nil {
		t.Errorf("NewDataSource(nil) = %#v, want untyped nil", got)
	}
}

// TestNewDataSource_NonNilManager returns a usable adapter.
func TestNewDataSource_NonNilManager(t *testing.T) {
	t.Parallel()
	// NewManager without Scan returns an empty Manager; methods all
	// return nil / zero-value, which is fine for this smoke.
	m := &Manager{projects: map[string]*Project{}}
	ds := NewDataSource(m)
	if ds == nil {
		t.Fatal("NewDataSource with non-nil Manager returned nil")
	}

	// Unbound chat → zero ProjectBinding
	b := ds.ProjectBinding("feishu", "direct", "alice")
	if b.Bound {
		t.Errorf("expected Bound=false for empty manager, got %+v", b)
	}

	// Missing project → ok=false
	if _, ok := ds.ProjectByName("missing"); ok {
		t.Error("expected ok=false for missing project")
	}
}

// TestPlannerKeyFor_MatchesSessionPackage is the cross-package format
// drift guard (docs/rfc/key-resolver.md §3.5). session.plannerKeyFor
// is unexported, so this test lives in project (which imports session)
// and exercises project.PlannerKeyFor against a hardcoded literal that
// session_test also asserts. Both endpoints locking the same literal
// without a direct cross-call keeps the import graph acyclic.
func TestPlannerKeyFor_FormatLocked(t *testing.T) {
	t.Parallel()
	// MUST match session.plannerKeyFor's literal in routing_test.go
	// TestPlannerKeyFor_Format. Format migration = update both.
	if got := PlannerKeyFor("foo"); got != "project:foo:planner" {
		t.Errorf("PlannerKeyFor(foo) = %q, want %q", got, "project:foo:planner")
	}
}

// TestNewDataSource_Compiles is a smoke test that the adapter satisfies
// the session.PlannerDataSource interface. Interface satisfaction is
// checked at compile time, but this explicit assignment makes the
// contract obvious to future readers.
func TestNewDataSource_Compiles(t *testing.T) {
	t.Parallel()
	m := &Manager{projects: map[string]*Project{}}
	var _ session.PlannerDataSource = NewDataSource(m) // compile-time check
}
