// Package session_test — planner_key_contract_test.go
//
// Cross-package behavior-equality test for the two isPlannerKey
// implementations (R215-CR-P1-1). The session package re-implements
// project.IsPlannerKey locally to break the session→project import
// cycle (see internal/session/key.go isPlannerKey godoc). The hardcoded
// literal mirroring is locked from each side independently:
//
//   - internal/project/project_test.go::TestIsPlannerKey_*
//   - internal/session/routing_test.go::TestIsPlannerKey
//
// but no existing test runs the two implementations against the SAME
// input set. Drift in either direction (one accepts an edge case the
// other rejects) silently breaks any caller that crosses the boundary.
// This file fixes that by table-driving both with one input vector.
package session_test

import (
	"testing"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

func TestIsPlannerKey_CrossPackageContract(t *testing.T) {
	t.Parallel()
	cases := []string{
		"project:foo:planner",
		"project:my-project:planner",
		"project::planner",
		"project:foo",
		"cron:foo",
		"scratch:abc:general:general",
		"feishu:direct:alice:general",
		"",
		":planner",
		"project:",
		"project:foo:planner:extra",
		"project:foo:plannerx",
		"PROJECT:foo:planner",
	}
	for _, key := range cases {
		got := session.IsPlannerKey(key)
		want := project.IsPlannerKey(key)
		if got != want {
			t.Errorf("isPlannerKey divergence for %q: session=%v project=%v",
				key, got, want)
		}
	}
}
