package cron_test

// stop_policy_divergence_test.go pins the Sec-LOW-2 invariant that #1169
// surfaced: cron and sysession DELIBERATELY diverge on their Stop-overflow
// strategy, and that divergence is a security property, not an inconsistency
// to "harmonise". The two strategies are exposed as grep-able typed string
// constants (#1060 / R244-ARCH-7); this test fails loudly if either constant
// is deleted or if a well-meaning refactor collapses them to the same value
// (the exact "unify the lifecycle" move that Sec-LOW-2 forbids and that the
// rejected lifecycle.Manager RFC already tried).
//
// Kept behavioural-minimal: it asserts the constants exist, are non-empty, and
// differ. It does NOT pin their literal string values — operators may rename
// the wire strings, but cron must never adopt force-exit nor sysession adopt
// budget-then-leak.

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/sysession"
)

func TestStopPolicies_DivergeBetweenCronAndSysession(t *testing.T) {
	if cron.StopPolicyBudgetThenLeak == "" {
		t.Error("cron.StopPolicyBudgetThenLeak must be a non-empty documented policy constant")
	}
	if sysession.StopPolicyForceExit == "" {
		t.Error("sysession.StopPolicyForceExit must be a non-empty documented policy constant")
	}
	// The whole point of Sec-LOW-2: the two subsystems must NOT share a
	// Stop-overflow strategy. cron leaks-and-exits (safe: dispatch retry
	// re-resolves the session); sysession force-exits (a leaked goroutine
	// could echo user-prompt-derived output into another session's reply).
	if cron.StopPolicyBudgetThenLeak == sysession.StopPolicyForceExit {
		t.Errorf("cron and sysession Stop policies must diverge (Sec-LOW-2); both are %q — "+
			"do not harmonise without reopening Sec-LOW-2", cron.StopPolicyBudgetThenLeak)
	}
}
