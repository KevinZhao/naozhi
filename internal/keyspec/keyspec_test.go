package keyspec

import "testing"

// TestPlannerKeyFor_FormatLocked is the canonical format-lock for the
// planner key shape. The literal MUST match both
// internal/session/routing_test.go's plannerKeyFor("foo") assertion
// AND internal/project/project_test.go's TestPlannerKeyFor assertion;
// when those tests live in different packages they could only catch
// drift via cross-import scaffolding. Co-locating the assertion with
// the source of truth here makes the contract local: a literal
// change here requires updating the project + session callers in
// the same commit.
func TestPlannerKeyFor_FormatLocked(t *testing.T) {
	if got := PlannerKeyFor("foo"); got != "project:foo:planner" {
		t.Errorf("PlannerKeyFor(foo) = %q, want %q", got, "project:foo:planner")
	}
	if got := PlannerKeyFor("some-long-name"); got != "project:some-long-name:planner" {
		t.Errorf("PlannerKeyFor(some-long-name) = %q, want %q", got, "project:some-long-name:planner")
	}
}

func TestIsPlannerKey_Valid(t *testing.T) {
	cases := []string{
		"project:myapp:planner",
		"project:x:planner",
		"project:some-long-project-name:planner",
	}
	for _, key := range cases {
		if !IsPlannerKey(key) {
			t.Errorf("IsPlannerKey(%q) = false, want true", key)
		}
	}
}

func TestIsPlannerKey_Invalid(t *testing.T) {
	cases := []string{
		"",
		"project::planner",          // empty name
		"project:planner",           // missing name segment
		"project:foo",               // missing :planner suffix
		"feishu:p2p:user:agent",     // wrong namespace
		"cron:abcdef",               // wrong namespace
		"project:foo:planner:extra", // trailing token after :planner
	}
	for _, key := range cases {
		if IsPlannerKey(key) {
			t.Errorf("IsPlannerKey(%q) = true, want false", key)
		}
	}
}

func TestPlannerNameFromKey(t *testing.T) {
	cases := map[string]string{
		"project:foo:planner":       "foo",
		"project:my-app:planner":    "my-app",
		"project:a:planner":         "a",
		"project:long-name:planner": "long-name",
	}
	for key, want := range cases {
		if got := PlannerNameFromKey(key); got != want {
			t.Errorf("PlannerNameFromKey(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestPlannerKeyFor_RoundTrip locks the inverse property:
// PlannerNameFromKey(PlannerKeyFor(n)) == n for every name we
// expect callers to pass. A future format change must preserve this
// invariant or both project and session migration paths break.
func TestPlannerKeyFor_RoundTrip(t *testing.T) {
	names := []string{"foo", "x", "some-long-project-name", "a-b-c-d"}
	for _, name := range names {
		key := PlannerKeyFor(name)
		if !IsPlannerKey(key) {
			t.Errorf("IsPlannerKey(PlannerKeyFor(%q)) = false; round-trip broken", name)
			continue
		}
		if got := PlannerNameFromKey(key); got != name {
			t.Errorf("PlannerNameFromKey(PlannerKeyFor(%q)) = %q, want %q", name, got, name)
		}
	}
}
