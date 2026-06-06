// cron_router_adapter_test.go pins the boot-time session.AgentOpts →
// cron.AgentOpts projection (toCronAgentOpts) that survives in main after the
// router adapter + ordinal pin moved to internal/wireup (R260528-ARCH-23 /
// #1382). The adapter behaviour + ordinal-drift coverage now lives in
// internal/wireup/cron_router_adapter_test.go.

package main

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestToCronAgentOpts_ExtraArgsCloned verifies that mutating the session-side
// ExtraArgs after toCronAgentOpts returns does NOT corrupt the cron-side
// slice. Used at boot to translate cfg.Agents (session.AgentOpts) →
// cron.AgentOpts in the cron Scheduler's agents map.
func TestToCronAgentOpts_ExtraArgsCloned(t *testing.T) {
	t.Parallel()
	sessArgs := []string{"--a", "--b"}
	in := session.AgentOpts{
		Backend:   "kiro",
		Model:     "sonnet",
		Workspace: "/var/y",
		ExtraArgs: sessArgs,
		Exempt:    false,
	}
	out := toCronAgentOpts(in)
	if len(out.ExtraArgs) != 2 || out.ExtraArgs[0] != "--a" || out.ExtraArgs[1] != "--b" {
		t.Fatalf("ExtraArgs not copied: got %#v", out.ExtraArgs)
	}
	sessArgs[1] = "--mutated"
	if out.ExtraArgs[1] != "--b" {
		t.Errorf("ExtraArgs aliased: out[1] = %q after mutating session source, want %q",
			out.ExtraArgs[1], "--b")
	}
}

// TestToCronAgentOpts_NilExtraArgs ensures a nil/empty session ExtraArgs
// translates to a nil cron ExtraArgs, not an empty non-nil slice that could
// surprise downstream nil checks.
func TestToCronAgentOpts_NilExtraArgs(t *testing.T) {
	t.Parallel()
	out := toCronAgentOpts(session.AgentOpts{Backend: "claude"})
	if out.ExtraArgs != nil {
		t.Errorf("empty ExtraArgs: got %#v, want nil", out.ExtraArgs)
	}
}
